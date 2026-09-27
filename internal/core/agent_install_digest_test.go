// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"aead.dev/minisign"
	"github.com/cenvero/fleet/internal/update"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/ssh"
)

// prehashedSignature signs message the way the release pipeline's minisign
// does by default: over its BLAKE2b-512 digest.
func prehashedSignature(t *testing.T, priv minisign.PrivateKey, message []byte, trustedComment string) []byte {
	t.Helper()
	r := minisign.NewReader(bytes.NewReader(message))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	return r.SignWithComments(priv, trustedComment, "test")
}

func blake2b512(data []byte) []byte {
	sum := blake2b.Sum512(data)
	return sum[:]
}

func TestVerifyMinisignDigestMatchesMinisignVerify(t *testing.T) {
	t.Parallel()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("an agent release archive")
	sigText := prehashedSignature(t, priv, message, "cenvero-fleet fleet-agent v1.2.3 linux-amd64")
	if !minisign.Verify(pub, message, sigText) {
		t.Fatal("minisign itself rejects the fixture signature")
	}
	var sig minisign.Signature
	if err := sig.UnmarshalText(sigText); err != nil {
		t.Fatal(err)
	}
	if sig.Algorithm != minisign.HashEdDSA {
		t.Fatalf("fixture signature algorithm = %#x, want prehashed", sig.Algorithm)
	}
	digest := blake2b512(message)
	if !verifyMinisignDigest(pub, digest, sig) {
		t.Fatal("a valid signature of the digest was rejected")
	}

	flipped := append([]byte(nil), digest...)
	flipped[len(flipped)-1] ^= 1
	if verifyMinisignDigest(pub, flipped, sig) {
		t.Fatal("a signature verified another digest")
	}
	if verifyMinisignDigest(pub, digest[:32], sig) {
		t.Fatal("a truncated digest verified")
	}
	if verifyMinisignDigest(otherPub, digest, sig) {
		t.Fatal("a signature verified under another key")
	}
	recommented := sig
	recommented.TrustedComment = "cenvero-fleet fleet-agent v9.9.9 linux-amd64"
	if verifyMinisignDigest(pub, digest, recommented) {
		t.Fatal("an altered trusted comment verified")
	}
	resigned := sig
	resigned.Signature[0] ^= 1
	if verifyMinisignDigest(pub, digest, resigned) {
		t.Fatal("an altered signature verified")
	}

	// A legacy signature signs the file itself, so no digest verifies it.
	var legacy minisign.Signature
	if err := legacy.UnmarshalText(minisign.SignWithComments(priv, message, "c", "u")); err != nil {
		t.Fatal(err)
	}
	if verifyMinisignDigest(pub, digest, legacy) {
		t.Fatal("a legacy signature verified from a digest")
	}
	legacy.Algorithm = minisign.HashEdDSA
	if verifyMinisignDigest(pub, digest, legacy) {
		t.Fatal("a legacy signature relabelled as prehashed verified")
	}
}

// Published v2.4.3 release signatures, with the BLAKE2b-512 and SHA-256
// digests of the archives they sign, checked against the embedded release key.
var publishedAgentReleaseVectors = []struct {
	target     agentLinuxTarget
	size       int64
	blake2b512 string
	sha256     string
	signature  string
}{
	{
		target:     agentLinuxTargets[0],
		size:       3614015,
		blake2b512: "2d56ac4ec7984c859f0d196eceabcab5f2aa0a4b57590e445bf2cd2a7155e00a521b3dd7fb62d51bfc4ce30e6468b1731667a21f5bbfc3886ca6052e1a4836c1",
		sha256:     "1b5e4705842b16c6a0bc972dc016b66f763e22ae8a2bc91e9c81ae4d37dc2d53",
		signature: "untrusted comment: signature from minisign secret key\n" +
			"RURb53p9WTsWCJ+C6Qd70PgG+1BFWGCC3dHV8sCJDcsdR8ENVU6xbD19qvKIpD9uwDrmlm00Sr94/sWfSBdhCAyyAeA8vN8tdw4=\n" +
			"trusted comment: cenvero-fleet fleet-agent v2.4.3 linux-amd64\n" +
			"Il+UciylgCQBumXj75qNvBlq5BbDt6x6OxgrT6a8zr+oQlqMBJKfwL/oD5uXD5VgyhBxzBVjzWydcFZPLPF5Aw==\n",
	},
	{
		target:     agentLinuxTargets[1],
		size:       3280831,
		blake2b512: "fe7c05cb092a5a8b29d5f3560b46b1d42e36e326228f43e38402718216884f660ecd576c21e68ad85bdd7ccafda1829ea4978650e3cfb144efe316440d6b7882",
		sha256:     "9c8a7fee5c805b4f5c1820fa51785e037a69d5e4be75c8b0fed0276d03cb6300",
		signature: "untrusted comment: signature from minisign secret key\n" +
			"RURb53p9WTsWCE4TufpeGQ0CoPmBLwGjFZq4BcOs3op0Frhp3d6/j/s2UTPd/PALGOH7u1GTxHIFCh//wXnPp6ATbKO6Y3H3/go=\n" +
			"trusted comment: cenvero-fleet fleet-agent v2.4.3 linux-arm64\n" +
			"0eDTHz4Wfs3qZidOvMqllKPcTUWR7MHTMo/mRRsgWRaGAiPZw7I+7vVtTXs8MLA32+nvO68bEri+7M7sX2cRBw==\n",
	},
}

func TestPublishedReleaseSignaturesVerifyFromDigests(t *testing.T) {
	t.Parallel()
	for _, v := range publishedAgentReleaseVectors {
		url := "https://github.com/cenvero/fleet/releases/download/v2.4.3/fleet-agent_2.4.3_linux_" + v.target.arch + ".tar.gz"
		manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{
			"v2.4.3": {v.target.name: {URL: url, Signature: url + ".minisig", SHA256: v.sha256, Size: v.size}},
		}}
		selected, err := selectAgentRelease("v2.4.3", v.target, manifest)
		if err != nil {
			t.Fatal(err)
		}
		blake, _ := hex.DecodeString(v.blake2b512)
		sum, _ := hex.DecodeString(v.sha256)
		digests := agentArchiveDigests{size: v.size, blake2b512: blake, sha256: sum}
		if err := verifySelectedAgentReleaseDigests(selected, digests, []byte(v.signature), update.SigningPublicKey()); err != nil {
			t.Fatalf("%s: the published release does not verify from its digests: %v", v.target.name, err)
		}
		tampered := digests
		tampered.blake2b512 = append([]byte(nil), blake...)
		tampered.blake2b512[0] ^= 0x80
		if err := verifySelectedAgentReleaseDigests(selected, tampered, []byte(v.signature), update.SigningPublicKey()); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("%s: a different archive digest verified: %v", v.target.name, err)
		}
		// The arm64 signature must not pass for the amd64 archive, and so on.
		other := publishedAgentReleaseVectors[0]
		if other.target == v.target {
			other = publishedAgentReleaseVectors[1]
		}
		if err := verifySelectedAgentReleaseDigests(selected, digests, []byte(other.signature), update.SigningPublicKey()); err == nil {
			t.Fatalf("%s: the %s signature verified this archive", v.target.name, other.target.name)
		}
	}
}

// digestReleaseFixture is agentReleaseFixture with a prehashed signature and a
// binary big enough that it matters whether it crosses the connection.
func digestReleaseFixture(t *testing.T, target agentLinuxTarget, trustedComment string) (update.Manifest, []byte, []byte, string, []byte) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	binary := make([]byte, 1<<20)
	if _, err := rand.Read(binary); err != nil {
		t.Fatal(err)
	}
	archive := agentTestArchive(t, binary)
	sum := sha256.Sum256(archive)
	url := "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet-agent_1.2.3_linux_" + target.arch + ".tar.gz"
	manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{
		"v1.2.3": {target.name: {URL: url, Signature: url + ".minisig", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}},
	}}
	if trustedComment == "" {
		trustedComment = "cenvero-fleet fleet-agent v1.2.3 " + target.name
	}
	return manifest, archive, prehashedSignature(t, priv, archive, trustedComment), string(pubText), binary
}

func TestVerifySelectedAgentReleaseDigestsFailsClosed(t *testing.T) {
	t.Parallel()
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, _ := digestReleaseFixture(t, target, "")
	selected, err := selectAgentRelease("v1.2.3", target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	good := agentArchiveDigests{size: int64(len(archive)), blake2b512: blake2b512(archive), sha256: sum[:]}
	if err := verifySelectedAgentReleaseDigests(selected, good, signature, pubText); err != nil {
		t.Fatalf("valid digests rejected: %v", err)
	}

	short := good
	short.size--
	otherPub, _, _ := minisign.GenerateKey(rand.Reader)
	otherPubText, _ := otherPub.MarshalText()
	wrongBlake := good
	wrongBlake.blake2b512 = blake2b512(archive[:len(archive)-1])
	wrongSHA := good
	wrongSHA.sha256 = make([]byte, sha256.Size)
	for name, tc := range map[string]struct {
		digests   agentArchiveDigests
		signature []byte
		key       string
		want      string
	}{
		"short archive":     {short, signature, pubText, "size mismatch"},
		"other archive":     {wrongBlake, signature, pubText, "signature verification failed"},
		"garbage signature": {good, []byte("not a minisign signature"), pubText, "signature verification failed"},
		"other key":         {good, signature, string(otherPubText), "signature verification failed"},
		"manifest checksum": {wrongSHA, signature, pubText, "checksum mismatch"},
		"bad embedded key":  {good, signature, "not a key", "parse embedded minisign key"},
	} {
		if err := verifySelectedAgentReleaseDigests(selected, tc.digests, tc.signature, tc.key); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	// A correctly signed archive of another version or target is refused by
	// its trusted comment.
	_, bArchive, bSig, bPub, _ := digestReleaseFixture(t, target, "cenvero-fleet fleet-agent v1.2.3 linux-arm64")
	bSum := sha256.Sum256(bArchive)
	bSelected := selected
	bSelected.info.Size = int64(len(bArchive))
	bSelected.info.SHA256 = hex.EncodeToString(bSum[:])
	bDigests := agentArchiveDigests{size: int64(len(bArchive)), blake2b512: blake2b512(bArchive), sha256: bSum[:]}
	if err := verifySelectedAgentReleaseDigests(bSelected, bDigests, bSig, bPub); err == nil || !strings.Contains(err.Error(), "signature binding mismatch") {
		t.Fatalf("wrong trusted comment: err = %v", err)
	}

	// A signature over the whole file cannot be checked from a digest; the
	// caller falls back to reading the archive.
	_, legacyPriv, _ := minisign.GenerateKey(rand.Reader)
	legacy := minisign.SignWithComments(legacyPriv, archive, "cenvero-fleet fleet-agent v1.2.3 linux-amd64", "u")
	if err := verifySelectedAgentReleaseDigests(selected, good, legacy, pubText); err == nil || !strings.Contains(err.Error(), "does not sign a digest") {
		t.Fatalf("legacy signature: err = %v", err)
	}
}

func TestParseAgentArchiveReport(t *testing.T) {
	t.Parallel()
	b := strings.Repeat("ab", blake2b.Size)
	s := strings.Repeat("cd", sha256.Size)
	got, hashed, err := parseAgentArchiveReport([]byte("fleet-agent-archive 1234 " + b + " " + s + "\n"))
	if err != nil || !hashed || got.size != 1234 || hex.EncodeToString(got.blake2b512) != b || hex.EncodeToString(got.sha256) != s {
		t.Fatalf("got %+v hashed=%v err=%v", got, hashed, err)
	}
	if _, hashed, err := parseAgentArchiveReport([]byte("fleet-agent-archive-unhashed\n")); err != nil || hashed {
		t.Fatalf("unhashed report: hashed=%v err=%v", hashed, err)
	}
	for _, bad := range []string{
		"",
		"noise\nfleet-agent-archive 1234 " + b + " " + s,
		"fleet-agent-archive 1234 " + b,
		"fleet-agent-archive -1 " + b + " " + s,
		"fleet-agent-archive 0 " + b + " " + s,
		"fleet-agent-archive 12x " + b + " " + s,
		"fleet-agent-archive 1234 " + b[:126] + " " + s,
		"fleet-agent-archive 1234 " + b + " " + s[:62],
		"fleet-agent-archive 1234 " + s + " " + b,
		"fleet-agent-archive 1234 " + strings.Repeat("zz", blake2b.Size) + " " + s,
		"fleet-agent-archive-unhashed extra",
	} {
		if _, _, err := parseAgentArchiveReport([]byte(bad)); err == nil {
			t.Fatalf("report %q accepted", bad)
		}
	}
}

// fakeTargetTools makes a directory of stand-ins for a target's commands: a
// Linux x86_64 uname, a wget that serves the files in serveDir by URL base
// name, and a curl that always fails. Each name in broken becomes a command
// that fails, hiding the real one.
func fakeTargetTools(t *testing.T, serveDir string, broken ...string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { // #nosec G306 -- test stand-in for a target command
			t.Fatal(err)
		}
	}
	write("uname", "#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 1 ;; esac\n")
	write("wget", `#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) out=$2; shift 2 ;;
    -q) shift ;;
    -T) shift 2 ;;
    *) url=$1; shift ;;
  esac
done
src="`+serveDir+`/${url##*/}"
[ -f "$src" ] || exit 8
if [ -n "${FLEET_TEST_WGET_DELAY:-}" ]; then sleep "$FLEET_TEST_WGET_DELAY"; fi
cat "$src" > "$out"
`)
	write("curl", "#!/bin/sh\nexit 7\n")
	for _, name := range broken {
		write(name, "#!/bin/sh\nexit 127\n")
	}
	return dir
}

// shellTargetRunner runs remote commands with the local /bin/sh, as a target
// reached over SSH would, and counts the bytes they send back.
type shellTargetRunner struct {
	path     string
	calls    []string
	received int
}

func (r *shellTargetRunner) Output(ctx context.Context, command string, limit int64) ([]byte, error) {
	r.calls = append(r.calls, command)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+r.path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	r.received += len(out)
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("remote command stdout exceeds %d bytes", limit)
	}
	if err != nil {
		return out, fmt.Errorf("remote command failed: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}

// serveRelease puts a release where fakeTargetTools' wget serves it.
func serveRelease(t *testing.T, manifest update.Manifest, archive, signature []byte) string {
	t.Helper()
	dir := t.TempDir()
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var info update.BinaryInfo
	for _, targets := range manifest.AgentBinaries {
		for _, i := range targets {
			info = i
		}
	}
	for name, data := range map[string][]byte{
		"manifest.json":                      manifestData,
		filepath.Base(info.URL):              archive,
		filepath.Base(info.URL) + ".minisig": signature,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the target side runs a POSIX shell")
	}
}

// haveTool reports whether the local system can compute a digest with the
// given command, so a test can run only where the tool behaves like on a
// typical target.
func haveTool(args ...string) bool {
	return exec.Command(args[0], append(args[1:], "/dev/null")...).Run() == nil // #nosec G204 -- fixed test tool names
}

func TestAgentFetchCommandReportsDigestsWithEachTool(t *testing.T) {
	requirePOSIXShell(t)
	archive := agentTestArchive(t, []byte("the agent binary"))
	serveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(serveDir, "a.tar.gz"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	want := fmt.Sprintf("fleet-agent-archive %d %x %x", len(archive), blake2b512(archive), sum)
	all := []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}
	except := func(keep ...string) []string {
		var out []string
		for _, n := range all {
			found := false
			for _, k := range keep {
				found = found || n == k
			}
			if !found {
				out = append(out, n)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		needs  [][]string
		broken []string
	}{
		{"coreutils", [][]string{{"b2sum"}, {"sha256sum"}}, except("b2sum", "sha256sum")},
		{"openssl", [][]string{{"openssl", "dgst", "-blake2b512"}, {"openssl", "dgst", "-sha256"}}, except("openssl")},
		{"python3", [][]string{{"python3", "-c", "import hashlib"}}, except("python3")},
		{"b2sum and shasum", [][]string{{"b2sum"}, {"shasum", "-a", "256"}}, except("b2sum", "shasum")},
		{"all", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, need := range tc.needs {
				if !haveTool(need...) {
					t.Skipf("%v is not available here", need)
				}
			}
			stage := t.TempDir()
			archivePath := filepath.Join(stage, "agent-release.tar.gz")
			runner := &shellTargetRunner{path: fakeTargetTools(t, serveDir, tc.broken...) + ":/usr/bin:/bin"}
			out, err := runner.Output(context.Background(), buildRemoteAgentFetchCommand("https://example.com/a.tar.gz", int64(len(archive)), archivePath), 4<<10)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("report = %q, want %q", got, want)
			}
			if got, err := os.ReadFile(archivePath); err != nil || !bytes.Equal(got, archive) {
				t.Fatalf("archive on the target: %v (err %v)", len(got), err)
			}
			if info, _ := os.Stat(archivePath); info.Mode().Perm() != 0o600 {
				t.Fatalf("archive mode = %v, want 0600", info.Mode().Perm())
			}
			entries, _ := os.ReadDir(stage)
			if len(entries) != 1 {
				t.Fatalf("staging dir holds %d entries, want only the archive", len(entries))
			}
		})
	}

	// No working tool, or only one that computes the wrong digest: the target
	// says so instead of reporting a digest.
	lying := t.TempDir()
	if err := os.WriteFile(filepath.Join(lying, "b2sum"), []byte("#!/bin/sh\necho "+strings.Repeat("ab", blake2b.Size)+"  \"$1\"\n"), 0o755); err != nil { // #nosec G306 -- test stand-in
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"none":  fakeTargetTools(t, serveDir, all...) + ":/usr/bin:/bin",
		"lying": lying + ":" + fakeTargetTools(t, serveDir, "openssl", "python3") + ":/usr/bin:/bin",
	} {
		stage := t.TempDir()
		archivePath := filepath.Join(stage, "agent-release.tar.gz")
		runner := &shellTargetRunner{path: path}
		out, err := runner.Output(context.Background(), buildRemoteAgentFetchCommand("https://example.com/a.tar.gz", int64(len(archive)), archivePath), 4<<10)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.TrimSpace(string(out)); got != "fleet-agent-archive-unhashed" {
			t.Fatalf("%s: report = %q", name, got)
		}
		if got, err := os.ReadFile(archivePath); err != nil || !bytes.Equal(got, archive) {
			t.Fatalf("%s: the archive was not kept for reading back (err %v)", name, err)
		}
	}
}

func TestAgentFetchCommandRefusesAnOversizedDownload(t *testing.T) {
	requirePOSIXShell(t)
	serveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(serveDir, "big.tar.gz"), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	archivePath := filepath.Join(stage, "agent-release.tar.gz")
	runner := &shellTargetRunner{path: fakeTargetTools(t, serveDir) + ":/usr/bin:/bin"}
	if out, err := runner.Output(context.Background(), buildRemoteAgentFetchCommand("https://example.com/big.tar.gz", 1024, archivePath), 4<<10); err == nil {
		t.Fatalf("oversized download succeeded: %q", out)
	}
	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatalf("an oversized download left %d files behind", len(entries))
	}
}

// TestRemoteAgentDownloadCommandEndsWithTheDownload: the command's output ends
// as soon as the download does. The deadline watchdog's sleep used to keep it
// open for up to a second more, which an SSH session waits out.
func TestRemoteAgentDownloadCommandEndsWithTheDownload(t *testing.T) {
	requirePOSIXShell(t)
	serveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(serveDir, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommand("https://example.com/payload", 1<<20))
	cmd.Env = append(os.Environ(), "PATH="+fakeTargetTools(t, serveDir)+":/usr/bin:/bin")
	// Give the watchdog time to start its sleep before the download ends.
	cmd.Env = append(cmd.Env, "FLEET_TEST_WGET_DELAY=0.2")
	started := time.Now()
	out, err := cmd.Output()
	if err != nil || string(out) != "payload" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Fatalf("the output stayed open for %s after a quick download", elapsed)
	}
}

// TestRemoteAgentDownloadCommandStopsItsWatchdogReliably: a download that ends
// before the deadline does not wait for the watchdog to notice. On a loaded
// target a TERM sent right after the watchdog is forked can be lost, and the
// command then took the whole deadline to finish. Running with TERM ignored
// loses it every time.
func TestRemoteAgentDownloadCommandStopsItsWatchdogReliably(t *testing.T) {
	requirePOSIXShell(t)
	serveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(serveDir, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `trap "" TERM; exec /bin/sh -c "$FLEET_TEST_COMMAND"`)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeTargetTools(t, serveDir)+":/usr/bin:/bin",
		"FLEET_TEST_COMMAND="+buildRemoteAgentDownloadCommandWithTimeout("https://example.com/payload", 1<<20, 5))
	started := time.Now()
	out, err := cmd.Output()
	if err != nil || string(out) != "payload" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the command waited %s for its watchdog", elapsed)
	}
}

func TestAgentStageCommand(t *testing.T) {
	requirePOSIXShell(t)
	binary := []byte("#!/bin/sh\necho the agent\n")
	archive := agentTestArchive(t, binary)
	sum := sha256.Sum256(archive)
	good := agentArchiveDigests{size: int64(len(archive)), blake2b512: blake2b512(archive), sha256: sum[:]}
	path := fakeTargetTools(t, t.TempDir()) + ":/usr/bin:/bin"
	setup := func(t *testing.T, data []byte) (string, string) {
		stage := t.TempDir()
		archivePath := filepath.Join(stage, "agent-release.tar.gz")
		if err := os.WriteFile(archivePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return archivePath, filepath.Join(stage, "agent.bin")
	}

	t.Run("unpacks the verified binary", func(t *testing.T) {
		archivePath, dest := setup(t, archive)
		out, err := (&shellTargetRunner{path: path}).Output(context.Background(), buildRemoteAgentStageCommand(archivePath, dest, good), 4<<10)
		if err != nil || strings.TrimSpace(string(out)) != "fleet-agent-unpacked" {
			t.Fatalf("out = %q, err = %v", out, err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(got, binary) {
			t.Fatalf("staged binary = %q (err %v)", got, err)
		}
		if info, _ := os.Stat(dest); info.Mode().Perm() != 0o700 {
			t.Fatalf("staged binary mode = %v, want 0700", info.Mode().Perm())
		}
		if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 1 {
			t.Fatalf("staging dir holds %d entries, want only the binary", len(entries))
		}
	})

	t.Run("refuses an archive that changed", func(t *testing.T) {
		changed := append([]byte(nil), archive...)
		changed[len(changed)-1] ^= 1
		archivePath, dest := setup(t, changed)
		_, err := (&shellTargetRunner{path: path}).Output(context.Background(), buildRemoteAgentStageCommand(archivePath, dest, good), 4<<10)
		if err == nil || !strings.Contains(err.Error(), "changed after it was verified") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("a binary was staged from a changed archive (stat err %v)", err)
		}
	})

	t.Run("refuses a manifest checksum that does not match", func(t *testing.T) {
		archivePath, dest := setup(t, archive)
		wrong := good
		wrong.sha256 = make([]byte, sha256.Size)
		if _, err := (&shellTargetRunner{path: path}).Output(context.Background(), buildRemoteAgentStageCommand(archivePath, dest, wrong), 4<<10); err == nil {
			t.Fatal("staged with a wrong SHA-256")
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("a binary was staged")
		}
	})

	t.Run("refuses when it can no longer hash", func(t *testing.T) {
		archivePath, dest := setup(t, archive)
		noTools := fakeTargetTools(t, t.TempDir(), "b2sum", "openssl", "python3", "sha256sum", "shasum") + ":/usr/bin:/bin"
		_, err := (&shellTargetRunner{path: noTools}).Output(context.Background(), buildRemoteAgentStageCommand(archivePath, dest, good), 4<<10)
		if err == nil || !strings.Contains(err.Error(), "cannot hash") {
			t.Fatalf("err = %v", err)
		}
	})

	for name, tc := range map[string]struct {
		archive []byte
		broken  []string
	}{
		"without tar":               {archive, []string{"tar"}},
		"without fleet-agent":       {agentTestArchiveNamed(t, "README.md", []byte("readme")), nil},
		"with an empty fleet-agent": {agentTestArchive(t, nil), nil},
	} {
		t.Run("leaves the archive to read back "+name, func(t *testing.T) {
			archivePath, dest := setup(t, tc.archive)
			s := sha256.Sum256(tc.archive)
			d := agentArchiveDigests{size: int64(len(tc.archive)), blake2b512: blake2b512(tc.archive), sha256: s[:]}
			p := fakeTargetTools(t, t.TempDir(), tc.broken...) + ":/usr/bin:/bin"
			out, err := (&shellTargetRunner{path: p}).Output(context.Background(), buildRemoteAgentStageCommand(archivePath, dest, d), 4<<10)
			if err != nil || strings.TrimSpace(string(out)) != "fleet-agent-not-unpacked" {
				t.Fatalf("out = %q, err = %v", out, err)
			}
			if got, _ := os.ReadFile(archivePath); !bytes.Equal(got, tc.archive) {
				t.Fatal("the archive was not left in place")
			}
			if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 1 {
				t.Fatalf("staging dir holds %d entries, want only the archive", len(entries))
			}
		})
	}
}

func agentTestArchiveNamed(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAcquireVerifiedAgentReleaseKeepsTheArchiveOnTheTarget(t *testing.T) {
	requirePOSIXShell(t)
	if !haveTool("b2sum") && !haveTool("openssl", "dgst", "-blake2b512") && !haveTool("python3", "-c", "import hashlib") {
		t.Skip("no BLAKE2b-512 tool here")
	}
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, binary := digestReleaseFixture(t, target, "")
	stage := t.TempDir()
	dest := filepath.Join(stage, "agent.bin")
	runner := &shellTargetRunner{path: fakeTargetTools(t, serveRelease(t, manifest, archive, signature)) + ":/usr/bin:/bin"}

	got, staged, err := acquireVerifiedAgentRelease(context.Background(), runner, BootstrapAgentRelease{Version: "v1.2.3", DestinationPath: dest}, stage, pubText)
	if err != nil {
		t.Fatal(err)
	}
	if !staged || got != nil {
		t.Fatalf("staged = %v with %d bytes returned, want the binary left on the target", staged, len(got))
	}
	if onTarget, err := os.ReadFile(dest); err != nil || !bytes.Equal(onTarget, binary) {
		t.Fatalf("binary on the target differs (err %v)", err)
	}
	manifestData, _ := json.Marshal(manifest)
	if limit := len(manifestData) + len(signature) + 1024; runner.received > limit {
		t.Fatalf("the target sent back %d bytes, want at most %d (manifest, signature and report)", runner.received, limit)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls = %d, want uname, manifest, fetch, signature, unpack", len(runner.calls))
	}
	if entries, _ := os.ReadDir(stage); len(entries) != 1 {
		t.Fatalf("staging dir holds %d entries, want only the binary", len(entries))
	}
}

func TestAcquireVerifiedAgentReleaseFallsBackToReadingTheArchive(t *testing.T) {
	requirePOSIXShell(t)
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, binary := digestReleaseFixture(t, target, "")
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	legacyPubText, _ := pub.MarshalText()
	legacySig := minisign.SignWithComments(priv, archive, "cenvero-fleet fleet-agent v1.2.3 linux-amd64", "legacy")

	emptyBlake := blake2b.Sum512(nil)
	for name, tc := range map[string]struct {
		broken    []string
		signature []byte
		key       string
		lyingTool bool
	}{
		"no hash tool":     {broken: []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}, signature: signature, key: pubText},
		"no tar":           {broken: []string{"tar"}, signature: signature, key: pubText},
		"legacy signature": {signature: legacySig, key: string(legacyPubText)},
		// A tool that hashes empty input right but the archive wrong must not
		// fail the install: the archive read back decides.
		"a hash tool that gets the archive wrong": {signature: signature, key: pubText, lyingTool: true},
	} {
		t.Run(name, func(t *testing.T) {
			stage := t.TempDir()
			dest := filepath.Join(stage, "agent.bin")
			tools := fakeTargetTools(t, serveRelease(t, manifest, archive, tc.signature), tc.broken...)
			if tc.lyingTool {
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = /dev/null ]; then echo '%x  -'; else echo '%s  -'; fi\n", emptyBlake, strings.Repeat("ab", blake2b.Size))
				if err := os.WriteFile(filepath.Join(tools, "b2sum"), []byte(script), 0o755); err != nil { // #nosec G306 -- test stand-in
					t.Fatal(err)
				}
			}
			runner := &shellTargetRunner{path: tools + ":/usr/bin:/bin"}
			got, staged, err := acquireVerifiedAgentRelease(context.Background(), runner, BootstrapAgentRelease{Version: "v1.2.3", DestinationPath: dest}, stage, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			if staged || !bytes.Equal(got, binary) {
				t.Fatalf("staged = %v, returned %d bytes; want the verified binary returned for upload", staged, len(got))
			}
			if runner.received < len(archive) {
				t.Fatalf("the archive was not read back (%d bytes received)", runner.received)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("a binary was staged on the target in a fallback")
			}
		})
	}
}

func TestAcquireVerifiedAgentReleaseRefusesABadRelease(t *testing.T) {
	requirePOSIXShell(t)
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, _ := digestReleaseFixture(t, target, "")
	tampered := append([]byte(nil), archive...)
	tampered[len(tampered)/2] ^= 1
	_, _, rebound, reboundPub, _ := digestReleaseFixture(t, target, "cenvero-fleet fleet-agent v1.2.2 linux-amd64")
	wrongSum := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{"v1.2.3": {}}}
	for k, v := range manifest.AgentBinaries["v1.2.3"] {
		v.SHA256 = strings.Repeat("0", 64)
		wrongSum.AgentBinaries["v1.2.3"][k] = v
	}

	for name, tc := range map[string]struct {
		manifest  update.Manifest
		archive   []byte
		signature []byte
		key       string
		broken    []string
		want      string
	}{
		"tampered archive":            {manifest, tampered, signature, pubText, nil, "signature verification failed"},
		"tampered archive, no tools":  {manifest, tampered, signature, pubText, []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}, "signature verification failed"},
		"other key":                   {manifest, archive, signature, reboundPub, nil, "signature verification failed"},
		"signed for another version":  {manifest, archive, rebound, reboundPub, nil, "signature verification failed"},
		"manifest checksum":           {wrongSum, archive, signature, pubText, nil, "checksum mismatch"},
		"manifest checksum, no tools": {wrongSum, archive, signature, pubText, []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}, "checksum mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			stage := t.TempDir()
			dest := filepath.Join(stage, "agent.bin")
			runner := &shellTargetRunner{path: fakeTargetTools(t, serveRelease(t, tc.manifest, tc.archive, tc.signature), tc.broken...) + ":/usr/bin:/bin"}
			_, _, err := acquireVerifiedAgentRelease(context.Background(), runner, BootstrapAgentRelease{Version: "v1.2.3", DestinationPath: dest}, stage, tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("a binary was staged from a bad release")
			}
		})
	}
}

// TestSSHBootstrapInstallsAReleaseVerifiedOnTheTarget runs a whole agent
// release install over SSH against a target that executes each command with
// the local shell: the archive is downloaded, verified and unpacked on the
// target, and neither it nor the binary crosses the connection.
func TestSSHBootstrapInstallsAReleaseVerifiedOnTheTarget(t *testing.T) {
	requirePOSIXShell(t)
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, binary := digestReleaseFixture(t, target, "")
	serveDir := serveRelease(t, manifest, archive, signature)
	tampered := append([]byte(nil), archive...)
	tampered[len(tampered)/2] ^= 1
	tamperedDir := serveRelease(t, manifest, tampered, signature)
	for _, tc := range []struct {
		name     string
		serve    string
		broken   []string
		viaSSHUp bool   // the binary is uploaded by the controller
		wantErr  string // the install fails and installs nothing
	}{
		{name: "verified on the target", serve: serveDir},
		{name: "target without hash tools", serve: serveDir, broken: []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}, viaSSHUp: true},
		{name: "tampered download", serve: tamperedDir, wantErr: "signature verification failed"},
		{name: "tampered download, target without hash tools", serve: tamperedDir, broken: []string{"b2sum", "openssl", "python3", "sha256sum", "shasum"}, wantErr: "signature verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.broken) == 0 && !haveTool("b2sum") && !haveTool("openssl", "dgst", "-blake2b512") && !haveTool("python3", "-c", "import hashlib") {
				t.Skip("no BLAKE2b-512 tool here")
			}
			stage := filepath.Join(t.TempDir(), "cenvero-0123456789abcdef")
			installed := filepath.Join(t.TempDir(), "installed-agent")
			path := fakeTargetTools(t, tc.serve, tc.broken...) + ":/usr/bin:/bin"
			stats := &shellSSHStats{}
			clientConn, serverDone := startBootstrapTestSSHServer(t, serveShellOverSSH(t, path, stats))
			executor := sshBootstrapExecutor{
				networkDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
				releasePublicKey:   pubText,
			}
			req := bootstrapTestRequest(t)
			req.StagingDir = stage
			req.AgentRelease = &BootstrapAgentRelease{Version: "v1.2.3", DestinationPath: stage + "/agent.bin"}
			script := "#!/bin/sh\nset -eu\ncp " + shellQuote(stage+"/agent.bin") + " " + shellQuote(installed) + "\n"
			req.Uploads = []BootstrapUpload{{Path: stage + "/install.sh", Mode: 0o700, Content: []byte(script)}}
			req.RunCommand = "/bin/sh " + shellQuote(stage+"/install.sh")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			err := executor.Bootstrap(ctx, req)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Bootstrap: err = %v, want %q", err, tc.wantErr)
			}
			select {
			case err := <-serverDone:
				if err != nil {
					t.Fatalf("SSH test server: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SSH test server did not finish")
			}
			if _, err := os.Stat(stage); !os.IsNotExist(err) {
				t.Fatalf("staging dir left behind (stat err %v)", err)
			}
			if tc.wantErr != "" {
				if _, err := os.Stat(installed); !os.IsNotExist(err) {
					t.Fatal("a tampered release was installed")
				}
				return
			}
			if got, err := os.ReadFile(installed); err != nil || !bytes.Equal(got, binary) {
				t.Fatalf("installed binary differs (err %v)", err)
			}
			sent, received := stats.snapshot()
			if tc.viaSSHUp {
				if received < len(archive) || sent < len(binary) {
					t.Fatalf("fallback: received %d, sent %d; want the archive back and the binary up", received, sent)
				}
				return
			}
			if received > 64<<10 || sent > 64<<10 {
				t.Fatalf("received %d and sent %d bytes over SSH; the %d-byte archive or the binary crossed the connection", received, sent, len(archive))
			}
		})
	}
}

type shellSSHStats struct {
	mu       sync.Mutex
	sent     int // bytes the controller sent on stdin
	received int // bytes the target sent back on stdout
}

func (s *shellSSHStats) add(sent, received int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent += sent
	s.received += received
}

func (s *shellSSHStats) snapshot() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent, s.received
}

type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// serveShellOverSSH runs each exec request with the local /bin/sh and PATH,
// until the controller removes the staging directory.
func serveShellOverSSH(t *testing.T, path string, stats *shellSSHStats) func(*ssh.ServerConn, <-chan ssh.NewChannel) error {
	return func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		for newChannel := range channels {
			channel, requests, err := newChannel.Accept()
			if err != nil {
				return err
			}
			request, ok := <-requests
			if !ok || request.Type != "exec" {
				_ = channel.Close()
				return fmt.Errorf("expected exec request")
			}
			go ssh.DiscardRequests(requests)
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = channel.Close()
				return err
			}
			_ = request.Reply(true, nil)
			stdin := &countingReader{r: channel}
			stdout := &countingWriter{w: channel}
			cmd := exec.Command("/bin/sh", "-c", payload.Command)
			cmd.Env = append(os.Environ(), "PATH="+path)
			cmd.Stdin = stdin
			cmd.Stdout = stdout
			cmd.Stderr = channel.Stderr()
			status := uint32(0)
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Logf("run %q: %v", payload.Command, err)
				}
				status = 1
				if exitErr != nil && exitErr.ExitCode() > 0 {
					status = uint32(exitErr.ExitCode())
				}
			}
			stats.add(stdin.n, stdout.n)
			_ = channel.CloseWrite()
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			_ = channel.Close()
			if strings.HasPrefix(payload.Command, "rm -rf --") {
				return nil
			}
		}
		return nil
	}
}

// TestSSHBootstrapRefusesAnUploadOverTheReleaseArchive: the archive the target
// downloads into the staging directory has a fixed name no upload may take.
func TestSSHBootstrapRefusesAnUploadOverTheReleaseArchive(t *testing.T) {
	requirePOSIXShell(t)
	stage := filepath.Join(t.TempDir(), "cenvero-0123456789abcdef")
	clientConn, serverDone := startBootstrapTestSSHServer(t, serveShellOverSSH(t, "/usr/bin:/bin", &shellSSHStats{}))
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }}
	req := bootstrapTestRequest(t)
	req.StagingDir = stage
	req.AgentRelease = &BootstrapAgentRelease{Version: "v1.2.3", DestinationPath: stage + "/agent.bin"}
	req.Uploads = []BootstrapUpload{{Path: agentReleaseArchivePath(stage), Mode: 0o600, Content: []byte("x")}}
	req.RunCommand = "true"
	if err := executor.Bootstrap(context.Background(), req); err == nil || !strings.Contains(err.Error(), "collides with the agent release files") {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-serverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("SSH test server did not finish")
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("staging dir left behind (stat err %v)", err)
	}
}
