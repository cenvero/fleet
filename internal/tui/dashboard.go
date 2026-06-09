// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	fleetalerts "github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

func dashTabID(i int) string      { return fmt.Sprintf("dash-tab-%d", i) }
func dashRowID(tab, i int) string { return fmt.Sprintf("dash-row-%d-%d", tab, i) }

var (
	pageStyle = lipgloss.NewStyle().
			Padding(1, 2).
			Foreground(lipgloss.Color("#e7ecef")).
			Background(lipgloss.Color("#0a0e14"))

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00d4aa"))

	subtleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8fa7b3"))

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#5f7480"))

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#1c2b36")).
			Background(lipgloss.Color("#0d131b")).
			Padding(1, 2)

	tabStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Foreground(lipgloss.Color("#8fa7b3")).
			Background(lipgloss.Color("#101822"))

	activeTabStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Bold(true).
			Foreground(lipgloss.Color("#0a0e14")).
			Background(lipgloss.Color("#00d4aa"))

	selectedRowStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#0a0e14")).
				Background(lipgloss.Color("#c4fff2")).
				Bold(true)

	panelTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#e7ecef"))

	panelSubtleTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#0a0e14"))

	panelMetaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ab3bf"))

	criticalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff6b6b")).Bold(true)
	warningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffd166")).Bold(true)
	infoStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#74c0fc")).Bold(true)
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#00d4aa")).Bold(true)
)

type dashboardLoadedMsg struct {
	snapshot core.DashboardSnapshot
	err      error
}

type dashboardTab int

const (
	tabOverview dashboardTab = iota
	tabServers
	tabServices
	tabLogs
	tabAlerts
	tabOps
)

var dashboardTabs = []string{"Overview", "Servers", "Services", "Logs", "Alerts", "Ops"}

type serviceRow struct {
	Server    core.ServerRecord
	Service   core.ServiceRecord
	Reachable bool
}

type model struct {
	configDir    string
	snapshot     core.DashboardSnapshot
	err          error
	width        int
	height       int
	loading      bool
	activeTab    dashboardTab
	serverIndex  int
	serviceIndex int
	logIndex     int
	alertIndex   int
	auditIndex   int
}

func RunDashboard(configDir string) error {
	// bubblezone records clickable zones marked during View and answers
	// InBounds queries on mouse events — robust hit-testing without manual
	// coordinate math.
	zone.NewGlobal()
	m := model{
		configDir: configDir,
		width:     120,
		height:    36,
		loading:   true,
		activeTab: tabOverview,
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return loadDashboardCmd(m.configDir)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case dashboardLoadedMsg:
		m.snapshot = msg.snapshot
		m.err = msg.err
		m.loading = false
		m.clampSelections()
		return m, nil
	case tea.MouseMsg:
		if m.handleMouse(msg) {
			return m, nil
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, loadDashboardCmd(m.configDir)
		case "tab", "right", "l":
			m.activeTab = dashboardTab((int(m.activeTab) + 1) % len(dashboardTabs))
			m.clampSelections()
			return m, nil
		case "shift+tab", "left", "h":
			m.activeTab = dashboardTab((int(m.activeTab) - 1 + len(dashboardTabs)) % len(dashboardTabs))
			m.clampSelections()
			return m, nil
		case "1", "2", "3", "4", "5", "6":
			m.activeTab = dashboardTab(msg.String()[0] - '1')
			m.clampSelections()
			return m, nil
		case "up", "k":
			m.moveSelection(-1)
			return m, nil
		case "down", "j":
			m.moveSelection(1)
			return m, nil
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.loading && m.snapshot.GeneratedAt.IsZero() {
		return zone.Scan(pageStyle.Render(panelStyle.Width(max(60, m.width-8)).Render("Fetching fleet status...")))
	}
	if m.err != nil && m.snapshot.GeneratedAt.IsZero() {
		return zone.Scan(pageStyle.Render(panelStyle.Width(max(60, m.width-8)).Render("Dashboard error: " + m.err.Error())))
	}

	sections := []string{
		renderHeader(m.snapshot, loadingStateLine(m.loading, m.err)),
		renderTabs(m.activeTab, m.width),
		renderActiveTab(m),
		subtleStyle.Render("1-6 switch tabs  tab/shift+tab move tabs  j/k or mouse wheel move selection  click tabs/items  r refresh  q quit"),
	}
	// zone.Scan records marked zones and strips markers; called once at root.
	return zone.Scan(pageStyle.Width(max(80, m.width)).Render(strings.Join(sections, "\n\n")))
}

func loadDashboardCmd(configDir string) tea.Cmd {
	return func() tea.Msg {
		app, err := core.Open(configDir)
		if err != nil {
			if errors.Is(err, core.ErrNotInitialized) {
				err = fmt.Errorf("%w; run `fleet init` first", err)
			}
			return dashboardLoadedMsg{err: err}
		}
		defer app.Close()

		snapshot, err := app.DashboardSnapshot()
		return dashboardLoadedMsg{
			snapshot: snapshot,
			err:      err,
		}
	}
}

func (m *model) moveSelection(delta int) {
	switch m.activeTab {
	case tabServers:
		m.serverIndex = clamp(m.serverIndex+delta, 0, max(len(m.snapshot.Servers)-1, 0))
	case tabServices:
		rows := aggregateServices(m.snapshot.Servers)
		m.serviceIndex = clamp(m.serviceIndex+delta, 0, max(len(rows)-1, 0))
	case tabLogs:
		m.logIndex = clamp(m.logIndex+delta, 0, max(len(m.snapshot.CachedLogs)-1, 0))
	case tabAlerts:
		m.alertIndex = clamp(m.alertIndex+delta, 0, max(len(m.snapshot.RecentAlerts)-1, 0))
	case tabOps:
		m.auditIndex = clamp(m.auditIndex+delta, 0, max(len(m.snapshot.RecentAudit)-1, 0))
	}
}

func (m *model) handleMouse(msg tea.MouseMsg) bool {
	switch {
	case msg.Button == tea.MouseButtonWheelUp && msg.Action == tea.MouseActionPress:
		m.moveSelection(-1)
		return true
	case msg.Button == tea.MouseButtonWheelDown && msg.Action == tea.MouseActionPress:
		m.moveSelection(1)
		return true
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		// Tabs (zones marked in renderTabs).
		for i := range dashboardTabs {
			if zone.Get(dashTabID(i)).InBounds(msg) {
				m.activeTab = dashboardTab(i)
				m.clampSelections()
				return true
			}
		}
		// Rows of the active tab (zones marked in each tab renderer). Only the
		// active tab's rows are marked each frame, so this never cross-hits.
		if n, set := m.activeListSetter(); set != nil {
			for i := range n {
				if zone.Get(dashRowID(int(m.activeTab), i)).InBounds(msg) {
					set(i)
					return true
				}
			}
		}
	}
	return false
}

// activeListSetter returns the active tab's row count and a setter for its
// selection index, or (0, nil) if the active tab has no selectable list.
func (m *model) activeListSetter() (int, func(int)) {
	switch m.activeTab {
	case tabServers:
		return len(m.snapshot.Servers), func(i int) { m.serverIndex = i }
	case tabServices:
		return len(aggregateServices(m.snapshot.Servers)), func(i int) { m.serviceIndex = i }
	case tabLogs:
		return len(m.snapshot.CachedLogs), func(i int) { m.logIndex = i }
	case tabAlerts:
		return len(m.snapshot.RecentAlerts), func(i int) { m.alertIndex = i }
	case tabOps:
		return len(m.snapshot.RecentAudit), func(i int) { m.auditIndex = i }
	}
	return 0, nil
}

func (m *model) clampSelections() {
	m.serverIndex = clamp(m.serverIndex, 0, max(len(m.snapshot.Servers)-1, 0))
	rows := aggregateServices(m.snapshot.Servers)
	m.serviceIndex = clamp(m.serviceIndex, 0, max(len(rows)-1, 0))
	m.logIndex = clamp(m.logIndex, 0, max(len(m.snapshot.CachedLogs)-1, 0))
	m.alertIndex = clamp(m.alertIndex, 0, max(len(m.snapshot.RecentAlerts)-1, 0))
	m.auditIndex = clamp(m.auditIndex, 0, max(len(m.snapshot.RecentAudit)-1, 0))
}

func renderHeader(snapshot core.DashboardSnapshot, loading string) string {
	headerChrome := panelStyle.Copy().
		BorderForeground(lipgloss.Color("#00d4aa")).
		Background(lipgloss.Color("#0b1416"))

	brand := lipgloss.JoinHorizontal(
		lipgloss.Center,
		lipgloss.NewStyle().Foreground(lipgloss.Color("#0a0e14")).Background(lipgloss.Color("#00d4aa")).Padding(0, 1).Bold(true).Render("CENVERO"),
		" ",
		lipgloss.NewStyle().Foreground(lipgloss.Color("#d8fff6")).Background(lipgloss.Color("#123137")).Padding(0, 1).Bold(true).Render("FLEET CONSOLE"),
	)
	meta := []string{
		statusBadge("v"+snapshot.Status.Version, "#1b2836", "#74c0fc"),
		statusBadge("alias "+snapshot.Status.Alias, "#16232c", "#8fa7b3"),
		statusBadge("mode "+string(snapshot.Status.DefaultMode), "#122a28", "#00d4aa"),
		statusBadge("db "+fmt.Sprint(snapshot.Status.DatabaseBackend), "#211d14", "#ffd166"),
		statusBadge("channel "+snapshot.Status.Channel, "#2a1a1a", "#ff8787"),
	}
	if !snapshot.GeneratedAt.IsZero() {
		meta = append(meta, "snapshot "+snapshot.GeneratedAt.Local().Format("2006-01-02 15:04:05"))
	}
	metaLine := lipgloss.JoinHorizontal(lipgloss.Center, meta...)
	body := brand + "\n" +
		titleStyle.Render("Command your fleet.") + "\n" +
		subtleStyle.Render("Operator-owned control plane for services, transport, alerts, and updates.") + "\n\n" +
		metaLine
	if loading != "" {
		body += "\n" + loading
	}
	return headerChrome.Render(body)
}

func loadingStateLine(loading bool, loadErr error) string {
	if loadErr != nil {
		return criticalStyle.Render("Refresh error: " + loadErr.Error())
	}
	if loading {
		return subtleStyle.Render("Refreshing dashboard...")
	}
	return ""
}

func renderTabs(active dashboardTab, width int) string {
	items := make([]string, 0, len(dashboardTabs))
	for i, label := range dashboardTabs {
		token := fmt.Sprintf("%d %s", i+1, label)
		styled := tabStyle.Render(token)
		if dashboardTab(i) == active {
			styled = activeTabStyle.Render(token)
		}
		items = append(items, zone.Mark(dashTabID(i), styled))
	}
	return panelStyle.Width(max(70, width-8)).Render(strings.Join(items, " "))
}

func renderActiveTab(m model) string {
	switch m.activeTab {
	case tabOverview:
		return renderOverviewTab(m.snapshot, m.width)
	case tabServers:
		return renderServersTab(m.snapshot, m.width, m.serverIndex)
	case tabServices:
		return renderServicesTab(m.snapshot, m.width, m.serviceIndex)
	case tabLogs:
		return renderLogsTab(m.snapshot, m.width, m.logIndex)
	case tabAlerts:
		return renderAlertsTab(m.snapshot, m.width, m.alertIndex)
	case tabOps:
		return renderOpsTab(m.snapshot, m.width, m.auditIndex)
	default:
		return renderOverviewTab(m.snapshot, m.width)
	}
}

func renderOverviewTab(snapshot core.DashboardSnapshot, width int) string {
	leftWidth := panelWidth(width, 0.42)
	rightWidth := max(36, width-leftWidth-12)

	summaryCard := renderPanel("Fleet Summary", "Live estate health and inventory", renderSummary(snapshot), "#00d4aa", leftWidth)
	alertCard := renderPanel("Alert Feed", "Most recent operational events", renderCompactAlerts(snapshot.RecentAlerts), "#ff6b6b", rightWidth)
	activityCard := renderPanel("Recent Activity", "Controller audit trail", renderCompactAudit(snapshot.RecentAudit), "#74c0fc", rightWidth)
	fleetCard := renderPanel("Fleet Profile", "Update, key, and platform posture", renderOverviewDetails(snapshot), "#ffd166", leftWidth)

	if width >= 120 {
		top := lipgloss.JoinHorizontal(lipgloss.Top, summaryCard, "  ", alertCard)
		bottom := lipgloss.JoinHorizontal(lipgloss.Top, fleetCard, "  ", activityCard)
		return strings.Join([]string{top, bottom}, "\n\n")
	}
	return strings.Join([]string{summaryCard, alertCard, fleetCard, activityCard}, "\n\n")
}

func renderSummary(snapshot core.DashboardSnapshot) string {
	summary := snapshot.Summary
	lines := []string{
		metricLine(okStyle.Render("ONLINE"), summary.OnlineServers),
		metricLine(subtleStyle.Render("OFFLINE"), summary.OfflineServers),
		metricLine(criticalStyle.Render("CRITICAL"), summary.CriticalAlerts),
		metricLine(warningStyle.Render("WARNING"), summary.WarningAlerts),
		metricLine(infoStyle.Render("INFO"), summary.InfoAlerts),
		"",
		metricLine(subtleStyle.Render("SERVERS"), len(snapshot.Servers)),
		metricLine(subtleStyle.Render("TRACKED SERVICES"), countTrackedServices(snapshot.Servers)),
		metricLine(subtleStyle.Render("TEMPLATES"), len(snapshot.Templates)),
	}
	return strings.Join(lines, "\n")
}

func renderOverviewDetails(snapshot core.DashboardSnapshot) string {
	lines := []string{
		keyValueLine("Update channel", snapshot.Status.Channel),
		keyValueLine("Update policy", fmt.Sprint(snapshot.Status.Policy)),
		keyValueLine("Rollback ready", yesNo(snapshot.RollbackAvailable)),
		keyValueLine("Fingerprints", fmt.Sprintf("%d keys", len(snapshot.Status.Fingerprints))),
	}
	if len(snapshot.Servers) > 0 {
		server := snapshot.Servers[0]
		lines = append(lines,
			"",
			mutedStyle.Render("Featured server"),
			fmt.Sprintf("%s  %s", panelTitleStyle.Render(server.Name), subtleStyle.Render(string(server.Mode))),
			subtleStyle.Render(server.Address),
		)
	}
	return strings.Join(lines, "\n")
}

func renderCompactAlerts(recent []fleetalerts.Alert) string {
	lines := []string{}
	if len(recent) == 0 {
		lines = append(lines, subtleStyle.Render("No active alerts right now."))
		return strings.Join(lines, "\n")
	}
	for _, alert := range recent {
		state := "open"
		if alert.AcknowledgedAt != nil {
			state = "acked"
		}
		lines = append(lines,
			fmt.Sprintf("%-10s %-6s %s", styleSeverity(alert.Severity), statusBadge(strings.ToUpper(state), "#17212b", "#8fa7b3"), truncate(alert.Message, 48)),
			subtleStyle.Render("  "+dashIfEmpty(alert.Server)+"  ·  "+relativeTime(alert.CreatedAt)),
		)
	}
	return strings.Join(lines, "\n")
}

func renderCompactAudit(entries []logs.AuditEntry) string {
	lines := []string{}
	if len(entries) == 0 {
		lines = append(lines, subtleStyle.Render("No audit activity yet."))
		return strings.Join(lines, "\n")
	}
	for _, entry := range entries {
		lines = append(lines,
			fmt.Sprintf("%s  %s", statusBadge(entry.Timestamp.Local().Format("15:04"), "#16232c", "#8fa7b3"), truncate(entry.Action, 24)),
			subtleStyle.Render("  "+truncate(entry.Target, 48)),
		)
	}
	return strings.Join(lines, "\n")
}

func renderServersTab(snapshot core.DashboardSnapshot, width, selected int) string {
	lines := []string{"Servers"}
	if len(snapshot.Servers) == 0 {
		lines = append(lines, "", subtleStyle.Render("No servers added yet. Use `fleet server add` to grow the fleet."))
		return renderPanel("Servers", "Connected fleet inventory", strings.Join(lines, "\n"), "#00d4aa", max(70, width-8))
	}

	leftWidth := panelWidth(width, 0.42)
	rightWidth := max(40, width-leftWidth-12)

	server := snapshot.Servers[selected]
	listLines := []string{"Fleet Servers", ""}
	for i, item := range snapshot.Servers {
		row := fmt.Sprintf("%-16s %-8s %-8s %s", item.Name, shortStatus(item.Observed.Reachable), statusBadge(string(item.Mode), "#17212b", "#8fa7b3"), dashIfEmpty(item.Observed.NodeName))
		if i == selected {
			row = selectedRowStyle.Render(row)
		}
		listLines = append(listLines, zone.Mark(dashRowID(int(tabServers), i), row))
	}

	detailLines := []string{
		fmt.Sprintf("%s", server.Name),
		"",
		fmt.Sprintf("Address: %s:%d", server.Address, server.Port),
		fmt.Sprintf("User: %s", server.User),
		fmt.Sprintf("Mode: %s", server.Mode),
		fmt.Sprintf("Reachable: %s", yesNo(server.Observed.Reachable)),
		fmt.Sprintf("Transport: %s", dashIfEmpty(server.Observed.Transport)),
		fmt.Sprintf("Node: %s", dashIfEmpty(server.Observed.NodeName)),
		fmt.Sprintf("Agent version: %s", dashIfEmpty(server.Observed.AgentVersion)),
		fmt.Sprintf("OS/arch: %s", dashIfEmpty(strings.Trim(strings.Join([]string{server.Observed.OS, server.Observed.Arch}, "/"), "/"))),
		fmt.Sprintf("Last seen: %s", dashIfEmpty(relativeTime(server.Observed.LastSeen))),
		fmt.Sprintf("Host key: %s", truncate(dashIfEmpty(server.Observed.HostKeyFingerprint), 52)),
		fmt.Sprintf("CPU: %.1f%%   Memory: %.1f%%   Disk: %.1f%%", server.Metrics.CPUPercent, server.Metrics.MemoryPercent, server.Metrics.DiskPercent),
		fmt.Sprintf("Open ports: %s", formatPorts(server.OpenPorts)),
		fmt.Sprintf("Firewall: %s", firewallSummary(server.Firewall)),
		fmt.Sprintf("Template: %s", dashIfEmpty(server.LastTemplate)),
	}
	if server.Observed.LastError != "" {
		detailLines = append(detailLines, criticalStyle.Render("Last error: "+server.Observed.LastError))
	}
	detailLines = append(detailLines, "", subtleStyle.Render("Tracked services"))
	if len(server.Services) == 0 {
		detailLines = append(detailLines, subtleStyle.Render("No tracked services yet."))
	} else {
		for _, service := range server.Services {
			detailLines = append(detailLines, formatServiceLine(service))
		}
	}

	left := renderPanel("Servers", "Connected fleet inventory", strings.Join(listLines, "\n"), "#00d4aa", leftWidth)
	right := renderPanel("Server Detail", "Runtime posture and tracked services", strings.Join(detailLines, "\n"), "#74c0fc", rightWidth)
	if width >= 120 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join([]string{left, right}, "\n\n")
}

func renderServicesTab(snapshot core.DashboardSnapshot, width, selected int) string {
	rows := aggregateServices(snapshot.Servers)
	if len(rows) == 0 {
		return renderPanel("Services", "Tracked service catalog", "No tracked services yet.", "#ffd166", max(70, width-8))
	}

	leftWidth := panelWidth(width, 0.48)
	rightWidth := max(36, width-leftWidth-12)
	row := rows[selected]

	listLines := []string{"Tracked Services", ""}
	for i, item := range rows {
		entry := fmt.Sprintf("%-14s %-20s %-10s", item.Server.Name, item.Service.Name, serviceStateChip(item.Service))
		if i == selected {
			entry = selectedRowStyle.Render(entry)
		}
		listLines = append(listLines, zone.Mark(dashRowID(int(tabServices), i), entry))
	}

	detailLines := []string{
		fmt.Sprintf("%s / %s", row.Server.Name, row.Service.Name),
		"",
		fmt.Sprintf("Reachable server: %s", yesNo(row.Reachable)),
		fmt.Sprintf("Critical: %s", yesNo(row.Service.Critical)),
		fmt.Sprintf("State: %s", serviceState(row.Service)),
		fmt.Sprintf("Load state: %s", dashIfEmpty(row.Service.LoadState)),
		fmt.Sprintf("Sub state: %s", dashIfEmpty(row.Service.SubState)),
		fmt.Sprintf("Last action: %s", dashIfEmpty(row.Service.LastAction)),
		fmt.Sprintf("Log path: %s", dashIfEmpty(row.Service.LogPath)),
	}
	if row.Service.Description != "" {
		detailLines = append(detailLines, fmt.Sprintf("Description: %s", row.Service.Description))
	}

	left := renderPanel("Services", "Tracked service catalog", strings.Join(listLines, "\n"), "#ffd166", leftWidth)
	right := renderPanel("Service Detail", "Operational state and logging", strings.Join(detailLines, "\n"), "#00d4aa", rightWidth)
	if width >= 120 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join([]string{left, right}, "\n\n")
}

func renderLogsTab(snapshot core.DashboardSnapshot, width, selected int) string {
	logs := snapshot.CachedLogs
	if len(logs) == 0 {
		return renderPanel("Logs", "Controller-cached service log tails", "No cached logs yet. Read or follow a tracked service log first to populate the local fleet cache.", "#f4a261", max(70, width-8))
	}

	leftWidth := panelWidth(width, 0.48)
	rightWidth := max(36, width-leftWidth-12)
	entry := logs[selected]

	listLines := []string{"Cached Logs", ""}
	for i, item := range logs {
		state := statusBadge("EMPTY", "#17212b", "#8fa7b3")
		if item.Available {
			state = statusBadge(fmt.Sprintf("%d lines", len(item.Lines)), "#16232c", "#74c0fc")
		}
		lastLine := "no cached lines yet"
		if item.Available && len(item.Lines) > 0 {
			lastLine = truncate(item.Lines[len(item.Lines)-1].Text, 26)
		}
		row := fmt.Sprintf("%-12s %-18s %s %s", item.Server, item.Service, state, subtleStyle.Render(lastLine))
		if i == selected {
			row = selectedRowStyle.Render(row)
		}
		listLines = append(listLines, zone.Mark(dashRowID(int(tabLogs), i), row))
	}

	detailLines := []string{
		fmt.Sprintf("%s / %s", entry.Server, entry.Service),
		"",
		fmt.Sprintf("Cached path: %s", dashIfEmpty(entry.Path)),
		fmt.Sprintf("Available: %s", yesNo(entry.Available)),
		fmt.Sprintf("Tail lines: %d", len(entry.Lines)),
		fmt.Sprintf("Truncated: %s", yesNo(entry.Truncated)),
		"",
		subtleStyle.Render("Recent cached output"),
	}
	if !entry.Available {
		detailLines = append(detailLines, subtleStyle.Render("No cached lines yet. Use live log reads or follow mode to warm the controller cache."))
	} else {
		for _, line := range entry.Lines {
			detailLines = append(detailLines, fmt.Sprintf("%5d  %s", line.Number, line.Text))
		}
	}

	left := renderPanel("Logs", "Controller-cached service log tails", strings.Join(listLines, "\n"), "#f4a261", leftWidth)
	right := renderPanel("Log Detail", "Recent cached lines for the selected service", strings.Join(detailLines, "\n"), "#74c0fc", rightWidth)
	if width >= 120 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join([]string{left, right}, "\n\n")
}

func renderAlertsTab(snapshot core.DashboardSnapshot, width, selected int) string {
	alerts := snapshot.RecentAlerts
	if len(alerts) == 0 {
		return renderPanel("Alerts", "Severity and acknowledgement view", "No alerts. The fleet is quiet right now.", "#ff6b6b", max(70, width-8))
	}

	leftWidth := panelWidth(width, 0.48)
	rightWidth := max(36, width-leftWidth-12)
	alert := alerts[selected]

	listLines := []string{"Alerts", ""}
	for i, item := range alerts {
		entry := fmt.Sprintf("%-10s %-14s %s", styleSeverity(item.Severity), dashIfEmpty(item.Server), truncate(item.Message, 36))
		if item.AcknowledgedAt != nil {
			entry += "  " + statusBadge("ACKED", "#17212b", "#8fa7b3")
		}
		if i == selected {
			entry = selectedRowStyle.Render(entry)
		}
		listLines = append(listLines, zone.Mark(dashRowID(int(tabAlerts), i), entry))
	}

	detailLines := []string{
		fmt.Sprintf("%s", styleSeverity(alert.Severity)),
		"",
		fmt.Sprintf("Server: %s", dashIfEmpty(alert.Server)),
		fmt.Sprintf("Created: %s", alert.CreatedAt.Local().Format("2006-01-02 15:04:05")),
		fmt.Sprintf("Age: %s", relativeTime(alert.CreatedAt)),
		fmt.Sprintf("Code: %s", dashIfEmpty(alert.Code)),
		fmt.Sprintf("Acknowledged: %s", ackState(alert)),
		"",
		alert.Message,
	}

	left := renderPanel("Alerts", "Severity and acknowledgement view", strings.Join(listLines, "\n"), "#ff6b6b", leftWidth)
	right := renderPanel("Alert Detail", "Escalation context", strings.Join(detailLines, "\n"), "#74c0fc", rightWidth)
	if width >= 120 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join([]string{left, right}, "\n\n")
}

func renderOpsTab(snapshot core.DashboardSnapshot, width, selected int) string {
	leftWidth := panelWidth(width, 0.42)
	rightWidth := max(36, width-leftWidth-12)
	audit := snapshot.RecentAudit

	summaryLines := []string{
		"Ops State",
		"",
		fmt.Sprintf("Channel: %s", snapshot.Status.Channel),
		fmt.Sprintf("Policy: %s", snapshot.Status.Policy),
		fmt.Sprintf("Rollback ready: %s", yesNo(snapshot.RollbackAvailable)),
		fmt.Sprintf("Templates: %d", len(snapshot.Templates)),
	}
	if len(snapshot.Templates) > 0 {
		summaryLines = append(summaryLines, "", subtleStyle.Render("Available templates"))
		for _, name := range snapshot.Templates {
			summaryLines = append(summaryLines, "• "+name)
		}
	}
	keys := sortedFingerprints(snapshot.Status.Fingerprints)
	summaryLines = append(summaryLines, "", subtleStyle.Render("Controller keys"))
	for _, line := range keys {
		summaryLines = append(summaryLines, line)
	}

	auditLines := []string{"Audit Trail", ""}
	if len(audit) == 0 {
		auditLines = append(auditLines, subtleStyle.Render("No audit activity yet."))
	} else {
		selected = clamp(selected, 0, len(audit)-1)
		for i, entry := range audit {
			row := fmt.Sprintf("%s  %-18s %s", entry.Timestamp.Local().Format("15:04"), entry.Action, truncate(entry.Target, 24))
			if i == selected {
				row = selectedRowStyle.Render(row)
			}
			auditLines = append(auditLines, zone.Mark(dashRowID(int(tabOps), i), row))
		}
		auditLines = append(auditLines, "", subtleStyle.Render("Selected entry"))
		entry := audit[selected]
		auditLines = append(auditLines,
			fmt.Sprintf("Operator: %s", dashIfEmpty(entry.Operator)),
			fmt.Sprintf("Target: %s", dashIfEmpty(entry.Target)),
		)
		if entry.Details != "" {
			auditLines = append(auditLines, fmt.Sprintf("Details: %s", entry.Details))
		}
	}

	left := renderPanel("Ops", "Update, key, and release posture", strings.Join(summaryLines, "\n"), "#74c0fc", leftWidth)
	right := renderPanel("Audit Trail", "Recent operator actions", strings.Join(auditLines, "\n"), "#00d4aa", rightWidth)
	if width >= 120 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}
	return strings.Join([]string{left, right}, "\n\n")
}

func aggregateServices(servers []core.ServerRecord) []serviceRow {
	rows := make([]serviceRow, 0)
	for _, server := range servers {
		for _, service := range server.Services {
			rows = append(rows, serviceRow{
				Server:    server,
				Service:   service,
				Reachable: server.Observed.Reachable,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Service.Critical != rows[j].Service.Critical {
			return rows[i].Service.Critical
		}
		if serviceState(rows[i].Service) != serviceState(rows[j].Service) {
			return serviceState(rows[i].Service) < serviceState(rows[j].Service)
		}
		if rows[i].Server.Name != rows[j].Server.Name {
			return rows[i].Server.Name < rows[j].Server.Name
		}
		return rows[i].Service.Name < rows[j].Service.Name
	})
	return rows
}

func countTrackedServices(servers []core.ServerRecord) int {
	total := 0
	for _, server := range servers {
		total += len(server.Services)
	}
	return total
}

func renderPanel(title, subtitle, body, accent string, width int) string {
	chrome := panelStyle.Copy().
		Width(width).
		BorderForeground(lipgloss.Color(accent))
	banner := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#0a0e14")).
		Background(lipgloss.Color(accent)).
		Padding(0, 1).
		Bold(true).
		Render(title)

	header := banner
	if strings.TrimSpace(subtitle) != "" {
		header += "\n" + panelMetaStyle.Render(subtitle)
	}
	return chrome.Render(header + "\n\n" + body)
}

func statusBadge(label, bg, fg string) string {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(fg)).
		Background(lipgloss.Color(bg)).
		Padding(0, 1).
		Bold(true).
		Render(label)
}

func metricLine(label string, value int) string {
	return fmt.Sprintf("%-18s %s", label, statusBadge(fmt.Sprintf("%d", value), "#15232c", "#d8fff6"))
}

func keyValueLine(label, value string) string {
	return fmt.Sprintf("%-18s %s", subtleStyle.Render(label), value)
}

func formatServiceLine(service core.ServiceRecord) string {
	line := fmt.Sprintf("%-18s %s", service.Name, serviceStateChip(service))
	if service.Critical {
		line += "  " + criticalStyle.Render("critical")
	}
	return line
}

func shortStatus(reachable bool) string {
	if reachable {
		return okStyle.Render("online")
	}
	return criticalStyle.Render("offline")
}

func firewallSummary(state core.FirewallState) string {
	value := "disabled"
	if state.Enabled {
		value = "enabled"
	}
	if len(state.Rules) == 0 {
		return value
	}
	return fmt.Sprintf("%s (%d rules)", value, len(state.Rules))
}

func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%d", port))
	}
	return strings.Join(parts, ", ")
}

func serviceState(service core.ServiceRecord) string {
	state := dashIfEmpty(service.ActiveState)
	if service.SubState != "" {
		state += "/" + service.SubState
	}
	return state
}

func serviceStateChip(service core.ServiceRecord) string {
	state := strings.ToUpper(serviceState(service))
	bg := "#17212b"
	fg := "#8fa7b3"
	switch {
	case strings.Contains(strings.ToLower(service.ActiveState), "active"):
		bg, fg = "#11322c", "#00d4aa"
	case strings.Contains(strings.ToLower(service.ActiveState), "failed"):
		bg, fg = "#34191b", "#ff6b6b"
	case strings.Contains(strings.ToLower(service.ActiveState), "activating"):
		bg, fg = "#332611", "#ffd166"
	}
	return statusBadge(state, bg, fg)
}

func ackState(alert fleetalerts.Alert) string {
	if alert.AcknowledgedAt == nil {
		return "no"
	}
	return alert.AcknowledgedAt.Local().Format("2006-01-02 15:04:05")
}

func sortedFingerprints(values map[string]string) []string {
	if len(values) == 0 {
		return []string{subtleStyle.Render("No key fingerprints available.")}
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, name := range keys {
		lines = append(lines, fmt.Sprintf("%s  %s", name, truncate(values[name], 40)))
	}
	return lines
}

func styleSeverity(severity any) string {
	label := fmt.Sprint(severity)
	switch label {
	case "critical":
		return criticalStyle.Render(strings.ToUpper(label))
	case "warning":
		return warningStyle.Render(strings.ToUpper(label))
	default:
		return infoStyle.Render(strings.ToUpper(label))
	}
}

func truncate(input string, width int) string {
	if width < 4 || len(input) <= width {
		return input
	}
	return input[:width-3] + "..."
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func relativeTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	delta := time.Since(ts)
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(delta.Hours()/24))
	}
}

func panelWidth(total int, ratio float64) int {
	return max(34, int(float64(max(total-10, 70))*ratio))
}

func yesNo(value bool) string {
	if value {
		return okStyle.Render("yes")
	}
	return subtleStyle.Render("no")
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
