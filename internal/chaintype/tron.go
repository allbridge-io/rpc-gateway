package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name:        Tron,
		PassThrough: true,
		Head:        tronHead,
		Doc:         "full Tron HTTP API; the health check is `POST /wallet/getblock` with `{\"detail\":false}`",
	})
}

// tronGetBlockHeaderOnly asks /wallet/getblock for the latest block without its
// transactions: a ~500-byte header instead of the 0.5-1 MB that getnowblock
// sends on mainnet, which a slow provider cannot always deliver within the
// check timeout. Supported since java-tron 4.7.2.
const tronGetBlockHeaderOnly = `{"detail":false}`

// tronBlock is the part of /wallet/getblock we care about. Tron reports most
// failures with HTTP 200 and an "Error" field.
type tronBlock struct {
	BlockHeader struct {
		RawData struct {
			Number uint64 `json:"number"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Error string `json:"Error"`
}

// tronHead reads the head of a Tron chain from the full node HTTP API.
func tronHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := postJSON(ctx, client, JoinURLPath(baseURL, "/wallet/getblock"), headers, []byte(tronGetBlockHeaderOnly))
	if err != nil {
		return 0, err
	}
	var parsed tronBlock
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("getblock: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.Error != "" {
		return 0, fmt.Errorf("getblock: %s", parsed.Error)
	}
	if parsed.BlockHeader.RawData.Number == 0 {
		return 0, fmt.Errorf("getblock: no block number in response (%s)", truncate(string(body), 200))
	}
	return parsed.BlockHeader.RawData.Number, nil
}
