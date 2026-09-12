package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrintRoutesTable_NoHubRoutesIsUnchanged is the negative-space assertion
// plan 031 AC1/P13 asks for: "the existing tests still pass" does not prove the
// output is unchanged, so the hub-less table is pinned character for character.
// A user who never touches hub mode must see exactly what they saw before.
func TestPrintRoutesTable_NoHubRoutesIsUnchanged(t *testing.T) {
	routes := []proxyd.RouteInfo{
		{
			Hostname: "api.local.dev", Port: 443, Protocol: "https",
			Target:     proxyd.ServiceTarget{Host: "localhost", Port: 3000},
			ProjectDir: "/home/dev/app", PID: 4242, Connected: true,
			RegisteredAt: time.Now(),
		},
	}

	stdout, _ := captureOutput(t, func() { printRoutesTable(routes) })
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	require.Len(t, lines, 3)

	assert.Equal(t, "HOSTNAME       PORT  PROTOCOL  TARGET          PROJECT        PID", lines[0])
	assert.Equal(t, "--------       ----  --------  ------          -------        ---", lines[1])
	assert.Equal(t, "api.local.dev  443   https     localhost:3000  /home/dev/app  4242", lines[2])
	assert.NotContains(t, stdout, "SOURCE")
	assert.NotContains(t, stdout, "tunnel")
}

// TestPrintRoutesTable_WithHubRouteAddsSourceColumn pins the conditional half
// of §4.2: once a hub route exists, the table gains SOURCE, renders the hub
// route's TARGET as the tunnel it really is (the backend is on the publisher's
// machine, not this one), and shows the composed "<origin>:<dir>" PROJECT key.
func TestPrintRoutesTable_WithHubRouteAddsSourceColumn(t *testing.T) {
	routes := []proxyd.RouteInfo{
		{
			Hostname: "api.local.dev", Port: 443, Protocol: "https",
			Target:     proxyd.ServiceTarget{Host: "localhost", Port: 3000},
			ProjectDir: "/home/dev/app", PID: 4242, Connected: true,
		},
		{
			Hostname: "auth.llt.test", Port: 443, Protocol: "https",
			Target:     proxyd.ServiceTarget{Host: "localhost", Port: 8000},
			ProjectDir: "popos:/home/dev/remote", PID: 99, Origin: "popos",
		},
	}

	stdout, _ := captureOutput(t, func() { printRoutesTable(routes) })

	assert.Contains(t, stdout, "SOURCE")
	// The local route keeps its plain target; only the hub route is a tunnel.
	assert.Regexp(t, `local\s+api\.local\.dev\s+443\s+https\s+localhost:3000\s+/home/dev/app\s+4242`, stdout)
	assert.Regexp(t, `hub\s+auth\.llt\.test\s+443\s+https\s+tunnel -> localhost:8000\s+popos:/home/dev/remote\s+99`, stdout)
}
