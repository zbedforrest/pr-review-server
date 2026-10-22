package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"pr-review-server/poller"
)

type gradingPoller struct {
	reviewAPITestPoller
	startErr error
	ran      chan struct{}
}

func (p *gradingPoller) RunDailyGrade(_ context.Context, _ time.Time, onStart func()) (poller.GradeReport, error) {
	if p.startErr != nil {
		return poller.GradeReport{}, p.startErr
	}
	onStart()
	close(p.ran)
	return poller.GradeReport{Graded: 1}, nil
}

func postGrade(server *Server, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, dailyGradePath, nil)
	req.Header.Set("X-Prism-Job-Token", token)
	w := httptest.NewRecorder()
	server.handleDailyGradeJob(w, req)
	return w
}

func TestDailyGrade_StartsARunInTheBackground(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	server.cfg.HealthJobToken, server.cfg.GraderEnabled = "secret", true
	p := &gradingPoller{ran: make(chan struct{})}
	server.poller = p

	w := postGrade(server, "secret")
	assert.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	select {
	case <-p.ran:
	case <-time.After(time.Second):
		t.Fatal("grading run never started")
	}
}

func TestDailyGrade_RefusesBadTokenDisabledAndConflicts(t *testing.T) {
	server, _ := newTestServer(t, "tester")
	server.cfg.HealthJobToken = "secret"
	server.poller = &gradingPoller{startErr: poller.ErrNotLeader}

	assert.Equal(t, http.StatusUnauthorized, postGrade(server, "nope").Code)
	assert.Equal(t, http.StatusServiceUnavailable, postGrade(server, "secret").Code, "GRADER_ENABLED is off")

	server.cfg.GraderEnabled = true
	assert.Equal(t, http.StatusConflict, postGrade(server, "secret").Code, "only the lease holder grades")
	server.poller = &gradingPoller{startErr: poller.ErrGraderBusy}
	assert.Equal(t, http.StatusConflict, postGrade(server, "secret").Code)
}
