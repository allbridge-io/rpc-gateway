//go:build testnet

package testnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

// config.ChainTypeSui is the config value of the Sui chain type.

func init() {
	registerChecks(config.ChainTypeSui, typeChecks{
		Verify: func(t *testing.T, e *env, key string, _ config.Chain) { testSui(t, e, key) },
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			resp, _ := suiGraphQL(t, e, "/"+key, `{ checkpoint { sequenceNumber } }`)
			return resp
		},
	})
}

// suiGraphQLResp is a GraphQL answer: Sui reports a bad query as HTTP 200
// with "errors", so the checks look at both fields.
type suiGraphQLResp struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

// suiGraphQL posts a query to /{key}/graphql and decodes the answer; a
// transport failure or a non-200 status fails the test.
func suiGraphQL(t *testing.T, e *env, path, query string) (*http.Response, suiGraphQLResp) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"query": query})
	resp, data := e.do(t, http.MethodPost, path+"/graphql", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
	}
	var parsed suiGraphQLResp
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("not a GraphQL answer: %s", truncate(data))
	}
	return resp, parsed
}

// suiData decodes the data of an answer that must not carry errors.
func suiData(t *testing.T, r suiGraphQLResp, v any) {
	t.Helper()
	if len(r.Errors) > 0 {
		t.Fatalf("graphql error: %s", r.Errors[0].Message)
	}
	if err := json.Unmarshal(r.Data, v); err != nil {
		t.Fatalf("unexpected data %s: %v", r.Data, err)
	}
}

func testSui(t *testing.T, e *env, key string) {
	path := "/" + key

	t.Run("chainIdentifier", func(t *testing.T) {
		resp, r := suiGraphQL(t, e, path, `{ chainIdentifier }`)
		var data struct {
			ChainIdentifier string `json:"chainIdentifier"`
		}
		suiData(t, r, &data)
		t.Logf("%s chainIdentifier=%s provider=%s", key, data.ChainIdentifier, resp.Header.Get("X-Rpc-Provider"))
		// The Base58 digest of the genesis checkpoint: 32 bytes, 43-44 characters.
		if n := len(data.ChainIdentifier); n < 43 || n > 44 {
			t.Errorf("chain identifier %q is not a Base58 digest", data.ChainIdentifier)
		}
	})

	t.Run("latest checkpoint", func(t *testing.T) {
		_, r := suiGraphQL(t, e, path, `{ checkpoint { sequenceNumber timestamp } }`)
		var data struct {
			Checkpoint struct {
				SequenceNumber uint64 `json:"sequenceNumber"`
				Timestamp      string `json:"timestamp"`
			} `json:"checkpoint"`
		}
		suiData(t, r, &data)
		seq := data.Checkpoint.SequenceNumber
		if seq == 0 {
			t.Fatal("checkpoint sequence number is zero")
		}
		var checked uint64
		for _, s := range e.gw.Chain(key).Manager.Status() {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		t.Logf("%s checkpoint=%d at %s health-check checkpoint=%d", key, seq, data.Checkpoint.Timestamp, checked)
		// Checkpoints come every ~250ms, so a health round some seconds old is
		// still within a few hundred of the live value.
		if diff := absDiff(seq, checked); diff > 2000 {
			t.Errorf("checkpoint via proxy %d differs from the health-check one %d by %d", seq, checked, diff)
		}
	})

	t.Run("reference gas price", func(t *testing.T) {
		_, r := suiGraphQL(t, e, path, `{ epoch { epochId referenceGasPrice } }`)
		var data struct {
			Epoch struct {
				EpochID           uint64 `json:"epochId"`
				ReferenceGasPrice string `json:"referenceGasPrice"`
			} `json:"epoch"`
		}
		suiData(t, r, &data)
		if data.Epoch.ReferenceGasPrice == "" || data.Epoch.ReferenceGasPrice == "0" {
			t.Errorf("reference gas price is %q in epoch %d", data.Epoch.ReferenceGasPrice, data.Epoch.EpochID)
		}
	})

	t.Run("client error passed through", func(t *testing.T) {
		// An unknown field is the client's mistake: Sui answers HTTP 200 with a
		// GraphQL validation error, which must reach the client as-is instead
		// of being turned into a 503 or rerouted to another provider.
		before := len(e.rec.Of(events.KindRerouted, ""))
		_, r := suiGraphQL(t, e, path, `{ notAField }`)
		if len(r.Errors) == 0 {
			t.Fatalf("expected a GraphQL error for an unknown field, got data %s", r.Data)
		}
		if !strings.Contains(r.Errors[0].Message, "notAField") {
			t.Errorf("unexpected error %s: %s", r.Errors[0].Extensions.Code, r.Errors[0].Message)
		}
		if after := len(e.rec.Of(events.KindRerouted, "")); after != before {
			t.Errorf("a client error must not cause a reroute: %d new reroutes", after-before)
		}
	})

	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			suiGraphQL(t, e, path, `{ checkpoint { sequenceNumber } }`)
		}
	})
}
