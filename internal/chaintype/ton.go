package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name:        TON,
		PassThrough: true,
		Head:        tonHead,
		Doc:         "toncenter: the v3 REST API and `POST /api/v2/jsonRPC`; the health check is `GET /api/v3/masterchainInfo`",
	})
}

// tonMasterchainInfo is the part of GET /api/v3/masterchainInfo we care about.
// The real answer is {"last":{...,"seqno":N,...},"first":{...}}; toncenter
// reports failures as {"error":"..."} with a 4xx/5xx status.
type tonMasterchainInfo struct {
	Last *struct {
		Seqno uint64 `json:"seqno"`
	} `json:"last"`
	Error string `json:"error"`
}

// tonHead reads the head of a TON chain: the seqno of the last masterchain
// block, as reported by the toncenter v3 REST API.
//
// The status code carries most failures (getJSON rejects anything but 200, and
// quotes the {"error":...} body in its message); the Error field is still
// checked so a 200 that only says "error" cannot be mistaken for a healthy
// node with seqno 0.
func tonHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := getJSON(ctx, client, JoinURLPath(baseURL, "/api/v3/masterchainInfo"), headers)
	if err != nil {
		return 0, fmt.Errorf("masterchainInfo: %w", err)
	}
	var parsed tonMasterchainInfo
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("masterchainInfo: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.Error != "" {
		return 0, fmt.Errorf("masterchainInfo: %s", parsed.Error)
	}
	if parsed.Last == nil || parsed.Last.Seqno == 0 {
		return 0, fmt.Errorf("masterchainInfo: no last.seqno in response (%s)", truncate(string(body), 200))
	}
	return parsed.Last.Seqno, nil
}
