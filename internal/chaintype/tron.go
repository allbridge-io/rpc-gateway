package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name:        "tron",
		PassThrough: true,
		Head:        tronHead,
		Doc:         "full Tron HTTP API; the health check is `POST /wallet/getnowblock`",
	})
}

// tronNowBlock is the part of /wallet/getnowblock we care about. Tron reports
// most failures with HTTP 200 and an "Error" field.
type tronNowBlock struct {
	BlockHeader struct {
		RawData struct {
			Number uint64 `json:"number"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Error string `json:"Error"`
}

// tronHead reads the head of a Tron chain from the full node HTTP API.
func tronHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := postJSON(ctx, client, JoinURLPath(baseURL, "/wallet/getnowblock"), headers, []byte("{}"))
	if err != nil {
		return 0, err
	}
	var parsed tronNowBlock
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("getnowblock: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.Error != "" {
		return 0, fmt.Errorf("getnowblock: %s", parsed.Error)
	}
	if parsed.BlockHeader.RawData.Number == 0 {
		return 0, fmt.Errorf("getnowblock: no block number in response (%s)", truncate(string(body), 200))
	}
	return parsed.BlockHeader.RawData.Number, nil
}
