package admin

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/0xProject/rpc-gateway/internal/rpcgateway"
	"github.com/gorilla/mux"
	"github.com/purini-to/zapmw"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v2"
)

type Server struct {
	server *http.Server
}

func (s *Server) Start() error {
	zap.L().Info("Administration server starting", zap.String("listenAddr", s.server.Addr))
	return s.server.ListenAndServe()
}

func (s *Server) Stop() error {
	return s.server.Close()
}

func NewServer(config AdminServerConfig, gateway *rpcgateway.RPCGateway) *Server {
	r := mux.NewRouter()

	r.Use(
		zapmw.WithZap(zap.L()),
		zapmw.Request(zapcore.InfoLevel, "request"),
		zapmw.Recoverer(zapcore.ErrorLevel, "recover", zapmw.RecovererDefault),
	)

    r.HandleFunc(config.BasePath + "/admin/auth/token", GenerateTokenPayload(config)).Methods("POST")

    adminRouter := r.PathPrefix(config.BasePath + "/admin").Subrouter()
	adminRouter.Use(AdminAuthGuard(config))

	adminRouter.HandleFunc("/targets/{name}", UpdateTargetHandler(gateway)).Methods("POST")
	adminRouter.HandleFunc("/targets", GetTargetsHandler(gateway)).Methods("GET")

    r.PathPrefix("/").Handler(DefaultHandler{})

    var port uint = 7926
	if config.Port != 0 {
		port = config.Port
	}

	return &Server{
		server: &http.Server{
			Handler:           r,
			Addr:              fmt.Sprintf(":%d", port),
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

type DefaultHandler struct{}
func (h DefaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    zap.L().Warn("Not found", zap.String("path", r.URL.Path))
    http.NotFound(w, r)
}
