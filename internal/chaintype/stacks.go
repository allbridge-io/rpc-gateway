package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name:        Stacks,
		PassThrough: true,
		Head:        stacksHead,
		Doc:         "Stacks via the Hiro API, which also serves the node's `/v2/*`; the health check is `GET /v2/info`",
	})
}

// stacksInfo is the part of /v2/info the gateway reads. The height is a pointer
// so that a body missing the field (an HTML error page from a CDN, a different
// API answering 200) is told apart from a node honestly reporting height 0.
type stacksInfo struct {
	StacksTipHeight *uint64 `json:"stacks_tip_height"`
}

// stacksHead reads the Stacks chain tip from /v2/info. A Stacks node serves that
// endpoint and the Hiro API proxies it unchanged, so the same check works for
// either kind of target. Hiro reports failures with a real HTTP status code and
// a JSON body, which getJSON already turns into an error.
func stacksHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := getJSON(ctx, client, JoinURLPath(baseURL, "/v2/info"), headers)
	if err != nil {
		return 0, fmt.Errorf("v2/info: %w", err)
	}
	var parsed stacksInfo
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("v2/info: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.StacksTipHeight == nil {
		return 0, fmt.Errorf("v2/info: no stacks_tip_height in response (%s)", truncate(string(body), 200))
	}
	return *parsed.StacksTipHeight, nil
}
