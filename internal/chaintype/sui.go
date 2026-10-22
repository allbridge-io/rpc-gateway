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
		Name:        Sui,
		PassThrough: true,
		Head:        suiHead,
		Doc:         "Sui GraphQL RPC (`POST /graphql`) and anything else the target serves under its base URL; the health check is `checkpoint.sequenceNumber` over GraphQL",
	})
}

// suiCheckpointQuery asks for the latest checkpoint only: a checkpoint without
// arguments is the newest one the server has indexed.
const suiCheckpointQuery = `{"query":"{ checkpoint { sequenceNumber } }"}`

// suiGraphQLResponse is the part of the GraphQL answer the gateway reads. The
// server reports a bad query as HTTP 200 with "errors" and "data": null, so the
// errors must be looked at before the data. Checkpoint is a pointer so that a
// body without it (an HTML page from a CDN, a different API answering 200) is
// told apart from a real checkpoint.
type suiGraphQLResponse struct {
	Data *struct {
		Checkpoint *struct {
			SequenceNumber json.RawMessage `json:"sequenceNumber"`
		} `json:"checkpoint"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// suiHead reads the head of a Sui network. Sui has no blocks: the checkpoint
// sequence number is what advances (a new checkpoint every ~250ms), so it is
// what the gateway compares between targets to detect a lagging one.
//
// Sui removed JSON-RPC from the full node in v1.82 (October 2026); GraphQL is
// served at /graphql under the provider's base URL (QuickNode, Mysten's
// graphql.<network>.sui.io), which is why the type passes the client's path
// through instead of pinning one endpoint.
func suiHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := postJSON(ctx, client, JoinURLPath(baseURL, "/graphql"), headers, []byte(suiCheckpointQuery))
	if err != nil {
		return 0, fmt.Errorf("graphql checkpoint: %w", err)
	}
	var parsed suiGraphQLResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("graphql checkpoint: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if len(parsed.Errors) > 0 {
		return 0, fmt.Errorf("graphql checkpoint: graphql error: %s", truncate(parsed.Errors[0].Message, 200))
	}
	if parsed.Data == nil || parsed.Data.Checkpoint == nil || len(parsed.Data.Checkpoint.SequenceNumber) == 0 {
		return 0, fmt.Errorf("graphql checkpoint: no checkpoint.sequenceNumber in response (%s)", truncate(string(body), 200))
	}
	seq, err := parseSuiU64(parsed.Data.Checkpoint.SequenceNumber)
	if err != nil {
		return 0, fmt.Errorf("graphql checkpoint: %w", err)
	}
	return seq, nil
}

// parseSuiU64 reads a Sui checkpoint number. GraphQL sends it as a JSON number
// (the UInt53 scalar); a decimal string, which is how Sui encodes a full u64
// elsewhere, is accepted as well, so a provider that answers with one is not
// reported as unhealthy for a difference that does not matter here.
func parseSuiU64(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		s = strings.TrimSpace(string(raw))
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected sequenceNumber %s", truncate(string(raw), 100))
	}
	return v, nil
}
