package admin

import (
    "encoding/json"
	"net/http"
	"strings"
)

type TargetInfo struct {
    Name        string `json:"name"`
    Disabled    bool   `json:"disabled"`
    BlockNumber uint64 `json:"blockNumber"`
}

func GetTargetsHandler(targetManager TargetManager) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
		var targetInfos []TargetInfo

        var targetConfigs = targetManager.GetTargetConfigs()
		for _, target := range targetConfigs {
			targetInfo := TargetInfo{
				Name:     target.Name,
				Disabled: target.IsDisabled,
				BlockNumber: targetManager.GetBlockNumberByName(target.Name),
			}

			targetInfos = append(targetInfos, targetInfo)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(targetInfos)
	}
}

func UpdateTargetHandler(targetManager TargetManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
        targetName := parts[len(parts)-1]
		if targetName == "" {
			http.Error(w, "Target name not provided", http.StatusBadRequest)
			return
		}

		found := targetManager.GetTargetConfigByName(targetName)
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

		targetManager.UpdateTargetStatus(found, *requestBody.Disabled)

		w.WriteHeader(http.StatusNoContent)
	}
}
