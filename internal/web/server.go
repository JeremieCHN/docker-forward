package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/JeremieCHN/docker-forward/internal/dockerx"
	"github.com/JeremieCHN/docker-forward/internal/rule"
)

//go:embed static
var staticFS embed.FS

var errNotFound = errors.New("rule not found")

// Server 提供控制面 REST API。所有写操作：校验 → 原子落盘 → 实时应用。
type Server struct {
	rulesPath string
	mgr       *rule.Manager
	dcli      *dockerx.Client
	rs        *dockerx.Resolver
	token     string
	version   string
	self      string // 自身容器名/ID（DOCKER_FORWARD_SELF 或容器内 hostname），用于禁止指向自身
	mux       *http.ServeMux
}

func NewServer(rulesPath string, mgr *rule.Manager, dcli *dockerx.Client, rs *dockerx.Resolver, token, version string) *Server {
	// host 网络模式下容器 hostname 与宿主机相同，需用 DOCKER_FORWARD_SELF 显式声明自身
	self := os.Getenv("DOCKER_FORWARD_SELF")
	if self == "" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			self, _ = os.Hostname()
		}
	}
	s := &Server{
		rulesPath: rulesPath,
		mgr:       mgr,
		dcli:      dcli,
		rs:        rs,
		token:     token,
		version:   version,
		self:      self,
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s
}

// checkSelfRef 禁止规则目标指向 docker-forward 自身容器（best-effort）。
func (s *Server) checkSelfRef(ctx context.Context, containerName string) error {
	if s.self == "" {
		return nil
	}
	if containerName == s.self {
		return fmt.Errorf("target container %q is docker-forward itself", containerName)
	}
	tid, err := s.dcli.ContainerID(ctx, containerName)
	if err != nil {
		return nil // 目标解析失败交给解析路径报错
	}
	sid, err := s.dcli.ContainerID(ctx, s.self)
	if err != nil {
		return nil
	}
	if tid == sid {
		return fmt.Errorf("target container %q is docker-forward itself", containerName)
	}
	return nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/rules", s.handleListRules)
	s.mux.HandleFunc("POST /api/rules", s.handleCreateRule)
	s.mux.HandleFunc("GET /api/rules/{id}", s.handleGetRule)
	s.mux.HandleFunc("PUT /api/rules/{id}", s.handleUpdateRule)
	s.mux.HandleFunc("DELETE /api/rules/{id}", s.handleDeleteRule)
	s.mux.HandleFunc("POST /api/rules/{id}/enable", s.handleSetEnabled(true))
	s.mux.HandleFunc("POST /api/rules/{id}/disable", s.handleSetEnabled(false))
	s.mux.HandleFunc("GET /api/containers", s.handleListContainers)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", http.FileServerFS(sub))
}

// Handler 静态资源公开，/api/ 路径在配置 token 时要求 Bearer 认证（UI 弹窗输入）。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			got := r.Header.Get("Authorization")
			want := "Bearer " + s.token
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or invalid token")
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

type ruleJSON struct {
	rule.Rule
	Status rule.RuleStatus `json:"status"`
}

func toRuleJSON(rws rule.RuleWithStatus) ruleJSON {
	return ruleJSON{Rule: rws.Rule, Status: rws.Status}
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	list := s.mgr.ListWithStatus()
	out := make([]ruleJSON, 0, len(list))
	for _, rws := range list {
		out = append(out, toRuleJSON(rws))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) handleGetRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, rws := range s.mgr.ListWithStatus() {
		if rws.Rule.ID == id {
			writeJSON(w, http.StatusOK, toRuleJSON(rws))
			return
		}
	}
	writeError(w, http.StatusNotFound, "rule not found")
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var in rule.Rule
	if !decodeBody(w, r, &in) {
		return
	}
	if in.ID == "" {
		in.ID = rule.NewID()
	}
	now := time.Now()
	in.CreatedAt = now
	in.UpdatedAt = now
	if in.Enabled {
		if err := s.checkSelfRef(r.Context(), in.ContainerName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.mutate(w, r, http.StatusCreated, func(cur []rule.Rule) ([]rule.Rule, error) {
		for _, e := range cur {
			if e.ID == in.ID {
				return nil, &rule.ConflictError{Msg: "rule id " + in.ID + " already exists"}
			}
		}
		next := append(append([]rule.Rule(nil), cur...), in)
		return rule.ValidateSet(next)
	}, in.ID)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in rule.Rule
	if !decodeBody(w, r, &in) {
		return
	}
	in.ID = id
	if in.Enabled {
		if err := s.checkSelfRef(r.Context(), in.ContainerName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.mutate(w, r, http.StatusOK, func(cur []rule.Rule) ([]rule.Rule, error) {
		next := make([]rule.Rule, 0, len(cur))
		found := false
		for _, e := range cur {
			if e.ID == id {
				in.CreatedAt = e.CreatedAt
				in.UpdatedAt = time.Now()
				next = append(next, in)
				found = true
				continue
			}
			next = append(next, e)
		}
		if !found {
			return nil, errNotFound
		}
		return rule.ValidateSet(next)
	}, id)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mutate(w, r, http.StatusOK, func(cur []rule.Rule) ([]rule.Rule, error) {
		next := make([]rule.Rule, 0, len(cur))
		found := false
		for _, e := range cur {
			if e.ID == id {
				found = true
				continue
			}
			next = append(next, e)
		}
		if !found {
			return nil, errNotFound
		}
		return next, nil
	}, "")
}

func (s *Server) handleSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if enabled {
			for _, e := range s.mgr.Rules() {
				if e.ID == id {
					if err := s.checkSelfRef(r.Context(), e.ContainerName); err != nil {
						writeError(w, http.StatusBadRequest, err.Error())
						return
					}
					break
				}
			}
		}
		s.mutate(w, r, http.StatusOK, func(cur []rule.Rule) ([]rule.Rule, error) {
			next := make([]rule.Rule, 0, len(cur))
			found := false
			for _, e := range cur {
				if e.ID == id {
					e.Enabled = enabled
					e.UpdatedAt = time.Now()
					found = true
				}
				next = append(next, e)
			}
			if !found {
				return nil, errNotFound
			}
			return rule.ValidateSet(next)
		}, id)
	}
}

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	infos, err := s.dcli.ListContainers(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "docker: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": infos})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dockerOK := s.dcli.Ping(ctx) == nil

	var counts = map[string]int{}
	for _, rws := range s.mgr.ListWithStatus() {
		counts[rws.Status.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.version,
		"docker": map[string]any{
			"host":      s.dcli.Host(),
			"connected": dockerOK,
		},
		"events": map[string]any{
			"state":      s.rs.State(),
			"lastSyncAt": s.rs.LastSyncAt(),
		},
		"rules": map[string]any{
			"total":    len(s.mgr.Rules()),
			"running":  counts[rule.StateRunning],
			"error":    counts[rule.StateError],
			"disabled": counts[rule.StateDisabled],
		},
	})
}

// mutate 执行一次规则集变更：fn 产出新规则集（含校验）→ 落盘 → 应用。
// ruleID 非空时响应附带该规则的最新状态。
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, okCode int, fn func(cur []rule.Rule) ([]rule.Rule, error), ruleID string) {
	next, err := fn(s.mgr.Rules())
	if err != nil {
		s.writeMutationError(w, err)
		return
	}
	if err := rule.SaveRules(s.rulesPath, next); err != nil {
		writeError(w, http.StatusInternalServerError, "save rules: "+err.Error())
		return
	}
	if err := s.mgr.Apply(next); err != nil {
		writeError(w, http.StatusInternalServerError, "apply rules: "+err.Error())
		return
	}
	if ruleID == "" {
		w.WriteHeader(okCode)
		return
	}
	for _, rws := range s.mgr.ListWithStatus() {
		if rws.Rule.ID == ruleID {
			writeJSON(w, okCode, toRuleJSON(rws))
			return
		}
	}
	w.WriteHeader(okCode)
}

func (s *Server) writeMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		var ce *rule.ConflictError
		if errors.As(err, &ce) {
			writeError(w, http.StatusConflict, ce.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
