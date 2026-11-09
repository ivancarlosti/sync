package google

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestMoveIntoSharedDriveUsesTheSelectedFolder pins the target of the move: a
// target folder replaces the shared-drive root as the addParents of every item,
// while removeParents stays the source root (the shared drive id, not the "root"
// alias, when the source is a shared drive).
func TestMoveIntoSharedDriveUsesTheSelectedFolder(t *testing.T) {
	transport := &reportTransport{bodies: map[string][]string{
		"/drive/v3/files": {`{"files":[{"id":"file-1","name":"Report","mimeType":"application/pdf",` +
			`"parents":["source-drive"],"size":"10"}]}`},
	}}
	provider := &Provider{httpClient: testHTTPClient(transport)}
	_, err := provider.MoveIntoSharedDrive(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"}, "source-drive", "target-drive", "target-folder", nil)
	if err != nil {
		t.Fatalf("MoveIntoSharedDrive = %v", err)
	}
	var patch string
	for _, raw := range transport.urls {
		if strings.Contains(raw, "/files/file-1") {
			patch = raw
		}
	}
	if patch == "" {
		t.Fatalf("no patch request issued (%v)", transport.urls)
	}
	for _, fragment := range []string{"addParents=target-folder", "removeParents=source-drive"} {
		if !strings.Contains(patch, fragment) {
			t.Errorf("patch = %q, want %q", patch, fragment)
		}
	}
}

// testHTTPClient wraps a transport in the client a provider expects.
func testHTTPClient(transport *reportTransport) *http.Client {
	return &http.Client{Transport: transport}
}
