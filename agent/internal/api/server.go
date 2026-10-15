// Package api is the local agent HTTP contract.
//
// Writes return 202 and a job id. Idempotency-Key replays the original
// accepted job. Config saves require If-Match and return 409 when the
// revision is stale or a manual edit conflicts. Logs are cursor-paged.
// Excess requests return 429 with Retry-After. The listener must be
// 127.0.0.1. See docs/API.md.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/backup"
	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/supervisor"
)

var (
	errForeignHost   = errors.New("foreign host")
	errForeignOrigin = errors.New("foreign origin")
)

// DefaultUIPort is the dashboard origin the API accepts when Options.UIPort is unset.
const DefaultUIPort = 5173

// instanceIDPattern is one leading letter or digit, then up to 63 more of those plus _ and -.
var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,63})$`)

// ValidateInstanceID rejects empty ids, path characters, and leading punctuation.
func ValidateInstanceID(id string) error {
	if !instanceIDPattern.MatchString(id) {
		return fmt.Errorf("invalid instance id %q", id)
	}
	return nil
}

// Options configures a local API server.
type Options struct {
	Token      string
	InstanceID string
	Instance   core.Instance
	Module     core.GameModule
	Editor     *config.Editor
	Jobs       *JobRunner
	Logs       *LogStore
	Limiter    *Limiter
	Idem       *IdempotencyStore
	Supervisor *supervisor.Supervisor
	Steam      supervisor.AppUpdater
	Backup     supervisor.BackupFunc
	// Archives stores backups. When set, and Backup is nil, update uses it.
	Archives *backup.Store
	// UserDir overrides the save directory named in the game config.
	// Empty means the module reads the config. A value here wins.
	UserDir      string
	ReadyTimeout time.Duration
	// UIPort is the only origin port a browser may send. Zero uses DefaultUIPort.
	UIPort int
}

// Server serves the local agent API.
type Server struct {
	tokenSum   [sha256.Size]byte
	uiPort     int
	instanceID string
	inst       core.Instance
	mod        core.GameModule
	editor     *config.Editor
	jobs       *JobRunner
	logs       *LogStore
	limiter    *Limiter
	idem       *IdempotencyStore
	sup        *supervisor.Supervisor
	steam      supervisor.AppUpdater
	backup     supervisor.BackupFunc
	archives   *backup.Store
	userDir    string
	acceptMu   sync.Mutex
	handler    http.Handler
}

// New builds a server. Token, module, editor, and instance id are required.
// The server does not open a socket; call Listen with 127.0.0.1.
func New(opt Options) (*Server, error) {
	if opt.Token == "" {
		return nil, errors.New("token is required")
	}
	if opt.Module == nil {
		return nil, errors.New("module is required")
	}
	if opt.Editor == nil {
		return nil, errors.New("editor is required")
	}
	if opt.InstanceID == "" {
		return nil, errors.New("instance id is required")
	}
	if opt.Jobs == nil {
		opt.Jobs = NewJobRunner(false)
	}
	if opt.Logs == nil {
		opt.Logs = NewLogStore()
	}
	if opt.Limiter == nil {
		opt.Limiter = NewLimiter(DefaultRateLimit, DefaultRateWindow)
	}
	if opt.Idem == nil {
		opt.Idem = NewIdempotencyStore()
	}
	uiPort := opt.UIPort
	if uiPort == 0 {
		uiPort = DefaultUIPort
	}
	if opt.Supervisor == nil {
		pol := supervisor.DefaultPolicy()
		if opt.ReadyTimeout > 0 {
			pol.ReadyTimeout = opt.ReadyTimeout
		}
		logs := opt.Logs
		opt.Supervisor = supervisor.New(pol, func(line string) {
			logs.Append(line)
		})
	}
	opt.Supervisor.Attach(opt.Instance, opt.Module)
	s := &Server{
		tokenSum:   sha256.Sum256([]byte(opt.Token)),
		uiPort:     uiPort,
		instanceID: opt.InstanceID,
		inst:       opt.Instance,
		mod:        opt.Module,
		editor:     opt.Editor,
		jobs:       opt.Jobs,
		logs:       opt.Logs,
		limiter:    opt.Limiter,
		idem:       opt.Idem,
		sup:        opt.Supervisor,
		steam:      opt.Steam,
		backup:     opt.Backup,
		archives:   opt.Archives,
		userDir:    opt.UserDir,
	}
	if s.backup == nil && s.archives != nil {
		s.backup = s.updateBackup
	}
	s.handler = s.wrap(s.routes())
	return s, nil
}

// Logs exposes the cursor log.
func (s *Server) Logs() *LogStore { return s.logs }

// Supervisor is the process host for this server.
func (s *Server) Supervisor() *supervisor.Supervisor { return s.sup }

// Handler returns the API handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ValidateAddr rejects anything other than a 127.0.0.1 host and a TCP port.
func ValidateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("refusing to bind %s: agent accepts 127.0.0.1 only", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("refusing to bind %s: invalid port", addr)
	}
	return nil
}

// Listen binds addr after ValidateAddr accepts it.
func Listen(addr string) (net.Listener, error) {
	if err := ValidateAddr(addr); err != nil {
		return nil, err
	}
	return net.Listen("tcp", addr)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/servers", s.listServers)
	mux.HandleFunc("GET /api/v1/servers/{instanceId}", s.getServer)
	mux.HandleFunc("GET /api/v1/servers/{instanceId}/status", s.getStatus)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/start", s.postStart)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/stop", s.postStop)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/restart", s.postRestart)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/install", s.postInstall)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/update", s.postUpdate)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/backup", s.postBackup)
	mux.HandleFunc("GET /api/v1/servers/{instanceId}/backups", s.getBackups)
	mux.HandleFunc("POST /api/v1/servers/{instanceId}/restore", s.postRestore)
	mux.HandleFunc("GET /api/v1/servers/{instanceId}/config", s.getConfig)
	mux.HandleFunc("PUT /api/v1/servers/{instanceId}/config", s.putConfig)
	mux.HandleFunc("GET /api/v1/servers/{instanceId}/logs", s.getLogs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.postCancelJob)
	return mux
}

func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, s.tokenSum) {
			if !s.allow(w, r) {
				return
			}
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized", Message: "bearer token required"})
			return
		}
		if !s.allow(w, r) {
			return
		}
		if err := checkRequest(r, s.uiPort); err != nil {
			if errors.Is(err, errForeignOrigin) {
				writeJSON(w, http.StatusForbidden, errorBody{Error: "forbidden_origin", Message: "foreign Origin is rejected"})
				return
			}
			writeJSON(w, http.StatusForbidden, errorBody{Error: "forbidden_host", Message: "agent accepts requests for 127.0.0.1 only"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allow(w http.ResponseWriter, r *http.Request) bool {
	ok, retry := s.limiter.Allow(clientIP(r))
	if ok {
		return true
	}
	sec := int((retry + time.Second - 1) / time.Second)
	if sec < 1 {
		sec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(sec))
	writeJSON(w, http.StatusTooManyRequests, errorBody{
		Error:   "rate_limited",
		Message: "too many requests",
	})
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func checkRequest(r *http.Request, uiPort int) error {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, "127.0.0.1") && !strings.EqualFold(host, "localhost") {
		return errForeignHost
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return errForeignOrigin
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errForeignOrigin
	}
	name := u.Hostname()
	if !strings.EqualFold(name, "127.0.0.1") && !strings.EqualFold(name, "localhost") {
		return errForeignOrigin
	}
	if u.Port() != strconv.Itoa(uiPort) {
		return errForeignOrigin
	}
	return nil
}

func authorized(r *http.Request, tokenSum [sha256.Size]byte) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(sum[:], tokenSum[:]) == 1
}
