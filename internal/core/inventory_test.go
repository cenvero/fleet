// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"reflect"
	"testing"
)

// fakeProber is a test double for inventoryProber.
type fakeProber struct {
	servers []ServerRecord
	out     map[string]string // server name -> probe stdout
	fail    map[string]error  // server name -> error to return
}

func (f fakeProber) ListServers() ([]ServerRecord, error) { return f.servers, nil }

func (f fakeProber) GetServer(name string) (ServerRecord, error) {
	for _, s := range f.servers {
		if s.Name == name {
			return s, nil
		}
	}
	return ServerRecord{}, errors.New("not found")
}

func (f fakeProber) ExecCommand(server, command string) (ExecResultLike, error) {
	if err := f.fail[server]; err != nil {
		return ExecResultLike{}, err
	}
	return ExecResultLike{Stdout: f.out[server]}, nil
}

func TestParseProbeOutput(t *testing.T) {
	item := &InventoryItem{}
	stdout := `HOSTNAME=web-01.example.com
OS=Ubuntu
OSVERSION=22.04
ARCH=x86_64
CPU=4
MEM=7.8Gi
DISKTOTAL=40G
DISKUSED=12G
IPS=10.0.0.5 203.0.113.7 192.168.1.2
PORTS=22 80 443 22
SERVICES=nginx.service ssh.service
`
	parseProbeOutput(item, stdout)

	if item.Hostname != "web-01.example.com" {
		t.Errorf("hostname = %q", item.Hostname)
	}
	if item.OS != "Ubuntu" || item.OSVersion != "22.04" {
		t.Errorf("os = %q/%q", item.OS, item.OSVersion)
	}
	if item.Arch != "x86_64" {
		t.Errorf("arch = %q", item.Arch)
	}
	if item.CPUCount != 4 {
		t.Errorf("cpu = %d", item.CPUCount)
	}
	if item.MemoryTotal != "7.8Gi" || item.DiskTotal != "40G" || item.DiskUsed != "12G" {
		t.Errorf("resources = %q %q %q", item.MemoryTotal, item.DiskTotal, item.DiskUsed)
	}
	if !reflect.DeepEqual(item.PublicIPs, []string{"203.0.113.7"}) {
		t.Errorf("public ips = %v", item.PublicIPs)
	}
	if !reflect.DeepEqual(item.PrivateIPs, []string{"10.0.0.5", "192.168.1.2"}) {
		t.Errorf("private ips = %v", item.PrivateIPs)
	}
	// Ports deduped and sorted.
	if !reflect.DeepEqual(item.ListenPorts, []int{22, 80, 443}) {
		t.Errorf("ports = %v", item.ListenPorts)
	}
	if !reflect.DeepEqual(item.Services, []string{"nginx.service", "ssh.service"}) {
		t.Errorf("services = %v", item.Services)
	}
}

func TestBuildInventory(t *testing.T) {
	dir := t.TempDir()
	tags := NewTagStore(dir)
	_ = tags.SetTags("web-01", map[string]string{"role": "plesk"})

	prober := fakeProber{
		servers: []ServerRecord{
			{Name: "web-01", Port: 22, Observed: ServerObservation{AgentVersion: "1.2.3"}},
			{Name: "db-01", Port: 2222},
		},
		out: map[string]string{
			"web-01": "HOSTNAME=web-01\nARCH=x86_64\nCPU=2\n",
		},
		fail: map[string]error{
			"db-01": errors.New("dial tcp: connection refused"),
		},
	}

	inv, err := BuildInventory(prober, tags, "")
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if inv.SchemaVersion != InventorySchemaVersion {
		t.Errorf("schema version = %d", inv.SchemaVersion)
	}
	if len(inv.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(inv.Items))
	}
	// Sorted by server name: db-01 then web-01.
	db, web := inv.Items[0], inv.Items[1]
	if db.Server != "db-01" || web.Server != "web-01" {
		t.Fatalf("unexpected order: %s, %s", db.Server, web.Server)
	}
	if web.Reachable != true || web.CPUCount != 2 || web.AgentPort != 22 || web.AgentVersion != "1.2.3" {
		t.Errorf("web-01 = %+v", web)
	}
	if !reflect.DeepEqual(web.Tags, map[string]string{"role": "plesk"}) {
		t.Errorf("web-01 tags = %v", web.Tags)
	}
	if db.Reachable != false || db.Error == "" || db.AgentPort != 2222 {
		t.Errorf("db-01 = %+v", db)
	}
}

func TestBuildInventorySingleServer(t *testing.T) {
	prober := fakeProber{
		servers: []ServerRecord{{Name: "web-01", Port: 22}},
		out:     map[string]string{"web-01": "HOSTNAME=h\n"},
	}
	inv, err := BuildInventory(prober, nil, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Items) != 1 || inv.Items[0].Server != "web-01" {
		t.Fatalf("items = %+v", inv.Items)
	}
}

func TestInventoryCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Inventory{
		SchemaVersion: InventorySchemaVersion,
		Items:         []InventoryItem{{Server: "web-01", Hostname: "h", Reachable: true}},
	}
	if err := SaveInventory(dir, want); err != nil {
		t.Fatalf("SaveInventory: %v", err)
	}
	got, err := LoadInventory(dir)
	if err != nil {
		t.Fatalf("LoadInventory: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Server != "web-01" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.1":     true,
		"192.168.1.1":  true,
		"172.16.0.1":   true,
		"172.32.0.1":   false,
		"203.0.113.10": false,
		"127.0.0.1":    true,
		"::1":          true,
		"fe80::1":      true,
	}
	for ip, want := range cases {
		if got := isPrivateIP(ip); got != want {
			t.Errorf("isPrivateIP(%q) = %v, want %v", ip, got, want)
		}
	}
}
