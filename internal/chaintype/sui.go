package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func init() {
	Register(Spec{
		Name: Sui,
		Head: suiHead,
		Doc:  "Sui JSON-RPC; the health check is `sui_getLatestCheckpointSequenceNumber`",
	})
}

// suiHead reads the head of a Sui network. Sui has no blocks: the checkpoint
// sequence number is what advances (a new checkpoint every ~250ms), so it is
// what the gateway compares between targets to detect a lagging one.
func suiHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	raw, err := callJSONRPC(ctx, client, baseURL, headers, "sui_getLatestCheckpointSequenceNumber", []any{})
	if err != nil {
		return 0, err
	}
	seq, err := parseSuiU64(raw)
	if err != nil {
		return 0, fmt.Errorf("sui_getLatestCheckpointSequenceNumber: %w", err)
	}
	return seq, nil
}

// parseSuiU64 reads a Sui u64. The API encodes u64 as a decimal *string*
// ("379726054") because a JSON number cannot carry the full range; a bare
// number is accepted as well, so a provider that answers with one is not
// reported as unhealthy for a difference that does not matter here.
func parseSuiU64(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		s = strings.TrimSpace(string(raw))
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected result %s", truncate(string(raw), 100))
	}
	return v, nil
}
