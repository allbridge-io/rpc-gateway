package admin

import (
    "encoding/json"
	"net/http"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/rpcgateway"
)

type TargetInfo struct {
    Name        string `json:"name"`
    Disabled    bool   `json:"disabled"`
    BlockNumber uint64 `json:"blockNumber"`
}

func GetTargetsHandler(rpcgateway *rpcgateway.RPCGateway) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
		var targetInfos []TargetInfo

        var targetConfigs = rpcgateway.GetTargetConfigs()
		for _, target := range targetConfigs {
			targetInfo := TargetInfo{
				Name:     target.Name,
				Disabled: target.IsDisabled,
				BlockNumber: rpcgateway.GetBlockNumberByName(target.Name),
			}

			targetInfos = append(targetInfos, targetInfo)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(targetInfos)
	}
}

func UpdateTargetHandler(rpcgateway *rpcgateway.RPCGateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		targetName := strings.TrimPrefix(r.URL.Path, "/admin/targets/")
		if targetName == "" {
			http.Error(w, "Target name not provided", http.StatusBadRequest)
			return
		}

		found := rpcgateway.GetTargetConfigByName(targetName)
		if found == nil {
			http.Error(w, "Target not found", http.StatusNotFound)
			return
		}

        var requestBody struct {
			Disabled *bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			http.Error(w, "Failed to decode JSON body", http.StatusBadRequest)
			return
		}
		if requestBody.Disabled == nil {
            http.Error(w, "Field 'disabled' is missing", http.StatusBadRequest)
            return
        }

		rpcgateway.UpdateTargetStatus(found, *requestBody.Disabled)

		w.WriteHeader(http.StatusNoContent)
	}
}
