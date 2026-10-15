package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/supervisor"
)

const maxBody = 1 << 20

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type decision struct {
	Status int
	Body   any
	JobID  string
	Store  bool
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"bindHost": "127.0.0.1",
	})
}

func (s *Server) listServers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": []map[string]string{s.instanceSummary()},
	})
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request) {
	if !s.knownInstance(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.instanceSummary())
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	if !s.knownInstance(w, r) {
		return
	}
	raw, err := os.ReadFile(s.editor.ConfigPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "could not read config"})
		return
	}
	if err != nil {
		raw = nil
	}
	st := s.sup.Status()
	var pid any
	if st.PID > 0 {
		pid = st.PID
	}
	body := map[string]any{
		"instanceId":    s.instanceID,
		"gameId":        s.mod.ID(),
		"state":         st.State,
		"ready":         st.Ready,
		"uptimeSeconds": uptimeSeconds(st),
		"pid":           pid,
		"ports":         s.mod.Ports(raw),
		"message":       st.Message,
		"restarts":      st.Restarts,
	}
	if st.MemoryKnown {
		body["memoryBytes"] = st.MemoryBytes
	}
	if st.CPUKnown {
		body["cpuPercent"] = st.CPUPercent
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) postStart(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, false, "start", s.startWork)
}

func (s *Server) postStop(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, true, "stop", s.stopWork)
}

func (s *Server) postRestart(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, true, "restart", s.restartWork)
}

func (s *Server) postInstall(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, false, "install", s.installWork)
}

func (s *Server) postUpdate(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{
			Error:   "not_implemented",
			Message: "update requires a backup hook; none is configured",
		})
		return
	}
	s.acceptAction(w, r, false, "update", s.updateWork)
}

func (s *Server) postBackup(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, false, "backup", func(context.Context) (any, error) {
		return map[string]any{"action": "backup", "implemented": false, "reason": "backups are not in this slice"}, nil
	})
}

func (s *Server) postRestore(w http.ResponseWriter, r *http.Request) {
	s.acceptAction(w, r, true, "restore", func(context.Context) (any, error) {
		return map[string]any{"action": "restore", "implemented": false, "reason": "restore is not in this slice"}, nil
	})
}

func (s *Server) startWork(ctx context.Context) (any, error) {
	return s.guardConfig("start", func() (any, error) {
		if err := s.sup.Start(ctx, s.inst, s.mod); err != nil {
			return nil, err
		}
		return s.processResult("start"), nil
	})
}

func (s *Server) stopWork(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.sup.Stop(ctx); err != nil {
		return nil, err
	}
	st := s.sup.Status()
	return map[string]any{"action": "stop", "state": st.State}, nil
}

func (s *Server) restartWork(ctx context.Context) (any, error) {
	return s.guardConfig("restart", func() (any, error) {
		if err := s.sup.Restart(ctx); err != nil {
			return nil, err
		}
		return s.processResult("restart"), nil
	})
}

func (s *Server) installWork(ctx context.Context) (any, error) {
	if s.steam == nil {
		return nil, errors.New("steamcmd is not configured")
	}
	return s.guardConfig("install", func() (any, error) {
		if err := s.sup.Install(ctx, s.steam); err != nil {
			return nil, err
		}
		st := s.sup.Status()
		return map[string]any{
			"action": "install",
			"state":  st.State,
			"exe":    s.mod.ExePath(s.inst),
		}, nil
	})
}

func (s *Server) updateWork(ctx context.Context) (any, error) {
	if s.steam == nil {
		return nil, errors.New("steamcmd is not configured")
	}
	if s.backup == nil {
		return nil, supervisor.ErrBackupNotConfigured
	}
	return s.guardConfig("update", func() (any, error) {
		if err := s.sup.Update(ctx, s.steam, s.backup); err != nil {
			return nil, err
		}
		return s.processResult("update"), nil
	})
}

func (s *Server) processResult(action string) map[string]any {
	st := s.sup.Status()
	return map[string]any{
		"action":         action,
		"exe":            s.mod.ExePath(s.inst),
		"args":           s.mod.LaunchArgs(s.inst),
		"state":          st.State,
		"processStarted": st.PID > 0 || st.Ready,
		"ready":          st.Ready,
		"pid":            st.PID,
	}
}

func (s *Server) guardConfig(action string, fn func() (any, error)) (any, error) {
	before, err := fileSHA256(s.editor.ConfigPath())
	if err != nil {
		return nil, err
	}
	result, err := fn()
	after, hashErr := fileSHA256(s.editor.ConfigPath())
	if hashErr != nil {
		return nil, hashErr
	}
	if before != after {
		return nil, fmt.Errorf("config was rewritten on %s", action)
	}
	return result, err
}

func uptimeSeconds(st supervisor.Status) int {
	var since time.Time
	if st.Ready && !st.ReadyAt.IsZero() {
		since = st.ReadyAt
	} else if st.PID > 0 && !st.StartedAt.IsZero() {
		since = st.StartedAt
	} else {
		return 0
	}
	sec := int(time.Since(since).Seconds())
	if sec < 0 {
		return 0
	}
	return sec
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	if !s.knownInstance(w, r) {
		return
	}
	res, err := s.editor.Read()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "could not read config"})
		return
	}
	warnings := s.mod.Warnings(res.Content)
	if warnings == nil {
		warnings = []string{}
	}
	body := map[string]any{
		"revision":       res.Revision,
		"sha256":         res.SHA256,
		"size":           res.Size,
		"externalChange": res.ExternalChange,
		"raw":            string(res.Content),
		"warnings":       warnings,
	}
	if !res.Mtime.IsZero() {
		body["mtime"] = res.Mtime.UTC()
	}
	if res.ExternalChange {
		body["lastKnown"] = string(res.LastKnown)
	}
	trimmed := bytes.TrimSpace(res.Content)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		body["content"] = json.RawMessage(trimmed)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	if !s.knownInstance(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_body", Message: bodyReadMessage(err)})
		return
	}
	s.accept(w, r, body, func() decision {
		rev, err := parseIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "if_match_required", Message: "If-Match revision is required"}}
		}
		var req struct {
			Content     json.RawMessage   `json:"content"`
			Resolutions map[string]string `json:"resolutions"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "invalid_body", Message: "body must be a JSON object"}}
		}
		if len(bytes.TrimSpace(req.Content)) == 0 {
			return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "invalid_body", Message: "content is required"}}
		}
		saved, err := s.editor.Save(rev, req.Content, req.Resolutions, func(merged []byte) error {
			errs := s.mod.Validate(merged)
			if len(errs) == 0 {
				return nil
			}
			return &validationError{Fields: errs}
		})
		if err != nil {
			return configDecision(err)
		}
		job := s.jobs.Record("config.save", s.instanceID, map[string]any{
			"revision": saved.Revision,
			"sha256":   saved.SHA256,
			"written":  saved.Written,
		}, nil)
		return decision{
			Status: http.StatusAccepted,
			Body:   map[string]string{"jobId": job.ID},
			JobID:  job.ID,
			Store:  true,
		}
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	if !s.knownInstance(w, r) {
		return
	}
	cursor, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_cursor", Message: "cursor must be an unsigned integer"})
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_limit", Message: "limit must be an integer from 1 to 500"})
		return
	}
	items, next := s.logs.Page(cursor, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"nextCursor": next,
	})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "unknown job"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) postCancelJob(w http.ResponseWriter, r *http.Request) {
	err := s.jobs.Cancel(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "unknown job"})
		return
	}
	if errors.Is(err, ErrJobFinished) {
		writeJSON(w, http.StatusConflict, errorBody{Error: "job_finished", Message: "job already finished"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "could not cancel job"})
		return
	}
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "unknown job"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) acceptAction(w http.ResponseWriter, r *http.Request, destructive bool, action string, work func(context.Context) (any, error)) {
	if !s.knownInstance(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_body", Message: bodyReadMessage(err)})
		return
	}
	s.accept(w, r, body, func() decision {
		if destructive {
			ok, err := confirmed(body)
			if err != nil {
				return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "invalid_body", Message: "body must be JSON"}}
			}
			if !ok {
				return decision{Status: http.StatusBadRequest, Body: errorBody{
					Error:   "confirm_required",
					Message: action + " requires confirm=true",
				}}
			}
		}
		job := s.jobs.Start(action, s.instanceID, work)
		return decision{
			Status: http.StatusAccepted,
			Body:   map[string]string{"jobId": job.ID},
			JobID:  job.ID,
			Store:  true,
		}
	})
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request, body []byte, run func() decision) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 200 {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_idempotency_key", Message: "Idempotency-Key must be 200 characters or fewer"})
		return
	}
	fp := fingerprint(r.Method, r.URL.RequestURI(), r.Header.Get("If-Match"), body)
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if key != "" {
		if rec, ok := s.idem.Lookup(key); ok {
			if rec.Fingerprint != fp {
				writeJSON(w, http.StatusConflict, errorBody{
					Error:   "idempotency_conflict",
					Message: "Idempotency-Key was already used with a different request",
				})
				return
			}
			writeBytes(w, rec.Status, rec.Body, rec.JobID)
			return
		}
	}
	decided := run()
	raw, err := json.Marshal(decided.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "could not encode response"})
		return
	}
	raw = append(raw, '\n')
	if key != "" && decided.Store && decided.Status == http.StatusAccepted {
		s.idem.Remember(key, idemRecord{
			Fingerprint: fp,
			Status:      decided.Status,
			Body:        raw,
			JobID:       decided.JobID,
		})
	}
	writeBytes(w, decided.Status, raw, decided.JobID)
}

func (s *Server) knownInstance(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("instanceId") == s.instanceID {
		return true
	}
	writeJSON(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "unknown instance"})
	return false
}

func (s *Server) instanceSummary() map[string]string {
	return map[string]string{
		"id":          s.instanceID,
		"gameId":      s.mod.ID(),
		"displayName": s.mod.DisplayName(),
	}
}

type validationError struct {
	Fields []core.FieldError
}

func (e *validationError) Error() string { return "validation failed" }

func configDecision(err error) decision {
	var stale *config.StaleError
	if errors.As(err, &stale) {
		return decision{Status: http.StatusConflict, Body: map[string]any{
			"error":    "stale_revision",
			"message":  "config revision does not match If-Match",
			"revision": stale.Revision,
		}}
	}
	var conflict *config.ConflictError
	if errors.As(err, &conflict) {
		return decision{Status: http.StatusConflict, Body: map[string]any{
			"error":     "conflict",
			"message":   "the file changed outside the agent; choose disk or edit for each conflicting field",
			"revision":  conflict.Revision,
			"conflicts": conflict.Conflicts,
		}}
	}
	var invalid *validationError
	if errors.As(err, &invalid) {
		return decision{Status: http.StatusBadRequest, Body: map[string]any{
			"error":   "validation_failed",
			"message": "config failed validation",
			"fields":  invalid.Fields,
		}}
	}
	var editErr *config.EditError
	if errors.As(err, &editErr) {
		return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "invalid_config", Message: editErr.Error()}}
	}
	if errors.Is(err, config.ErrExternalDelete) {
		return decision{Status: http.StatusConflict, Body: errorBody{
			Error:   "external_delete",
			Message: "server.json was removed outside the agent; refusing to recreate it",
		}}
	}
	if errors.Is(err, config.ErrInvalidResolution) {
		return decision{Status: http.StatusBadRequest, Body: errorBody{Error: "invalid_resolution", Message: err.Error()}}
	}
	var diskErr *config.DiskError
	if errors.As(err, &diskErr) {
		return decision{Status: http.StatusConflict, Body: errorBody{
			Error:   "external_invalid",
			Message: "on-disk config is not valid JSON and was left unchanged",
		}}
	}
	return decision{Status: http.StatusInternalServerError, Body: errorBody{Error: "internal", Message: "could not save config"}}
}

func confirmed(body []byte) (bool, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	var payload struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, err
	}
	return payload.Confirm, nil
}

func parseIfMatch(v string) (int64, error) {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	if strings.HasPrefix(v, "W/") {
		v = strings.Trim(strings.TrimPrefix(v, "W/"), `"`)
	}
	if v == "" {
		return 0, errors.New("missing")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid")
	}
	return n, nil
}

func parseCursor(v string) (uint64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseUint(v, 10, 64)
}

func parseLimit(v string) (int, error) {
	if v == "" {
		return 100, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 500 {
		return 0, errors.New("invalid limit")
	}
	return n, nil
}

var errBodyTooLarge = errors.New("body too large")

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func bodyReadMessage(err error) string {
	if errors.Is(err, errBodyTooLarge) {
		return "body is larger than 1 MiB"
	}
	return "could not read body"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeBytes(w, http.StatusInternalServerError, []byte(`{"error":"internal","message":"could not encode response"}`+"\n"), "")
		return
	}
	writeBytes(w, status, append(raw, '\n'), "")
}

func writeBytes(w http.ResponseWriter, status int, body []byte, jobID string) {
	if jobID != "" {
		w.Header().Set("Location", "/api/v1/jobs/"+jobID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return
	}
}
