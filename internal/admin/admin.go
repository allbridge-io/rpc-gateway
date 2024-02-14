package admin

import (
    "encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/0xProject/rpc-gateway/internal/rpcgateway"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

type Server struct {
	server *http.Server
}

type TargetInfo struct {
    Name        string `json:"name"`
    Disabled    bool   `json:"disabled"`
    BlockNumber uint64 `json:"blockNumber"`
}

func (s *Server) Start() error {
	zap.L().Info("Administration server starting", zap.String("listenAddr", s.server.Addr))
	return s.server.ListenAndServe()
}

func (s *Server) Stop() error {
	return s.server.Close()
}

func NewServer(config AdminServerConfig, r *rpcgateway.RPCGateway) *Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/admin/targets/", UpdateTargetHandler(r))

	mux.HandleFunc("/admin/targets", GetTargetsHandler(r))

	return &Server{
		server: &http.Server{
			Handler:           mux,
			Addr:              fmt.Sprintf(":%d", config.Port),
			WriteTimeout:      15 * time.Second,
			ReadTimeout:       15 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

func NewAdminServerConfigFromFile(path string) (*AdminServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return NewAdminServerConfigFromBytes(data)
}

func NewAdminServerConfigFromBytes(configBytes []byte) (*AdminServerConfig, error) {
	config := Config{}

	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		return nil, err
	}

	return &config.AdminServerConfig, nil
}

func NewAdminServerConfigFromString(configString string) (*AdminServerConfig, error) {
	return NewAdminServerConfigFromBytes([]byte(configString))
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

		response, err := json.Marshal(targetInfos)
		if err != nil {
			http.Error(w, "Failed to marshal JSON", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(response)
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
