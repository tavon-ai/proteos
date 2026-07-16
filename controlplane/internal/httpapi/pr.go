package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
)

// PR review endpoints (/api/git/repos/{owner}/{repo}/pulls/{number}...): the
// data layer of the mobile review loop. All five are CP→GitHub calls against
// the user's own token — deliberately independent of any machine or guest
// channel, so a PR stays reviewable and mergeable while its machine is stopped.
// Authorization is GitHub's: a repo the token cannot see is a uniform 404.

// prAuthorView is the PR author in prDetailView.
type prAuthorView struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// prDetailView is GET /api/git/repos/{owner}/{repo}/pulls/{number}. State is
// the review-surface state: open | draft | merged | closed.
type prDetailView struct {
	Number       int          `json:"number"`
	State        string       `json:"state"`
	Title        string       `json:"title"`
	Body         string       `json:"body,omitempty"`
	HTMLURL      string       `json:"html_url"`
	Head         string       `json:"head"`
	Base         string       `json:"base"`
	HeadSHA      string       `json:"head_sha"`
	Author       prAuthorView `json:"author"`
	Additions    int          `json:"additions"`
	Deletions    int          `json:"deletions"`
	ChangedFiles int          `json:"changed_files"`
}

// prReviewState folds GitHub's state/merged/draft triple into the single word
// the review surface renders.
func prReviewState(pr *github.PullRequestDetail) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.State == "closed":
		return "closed"
	case pr.Draft:
		return "draft"
	default:
		return "open"
	}
}

// handleGetPRDetail returns one PR's summary.
func (s *Server) handleGetPRDetail(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.prContext(w, r)
	if !ok {
		return
	}
	pr, err := s.GitHub.GetPR(r.Context(), rc.token, rc.owner, rc.repo, rc.number)
	if !writePRFetchOK(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, prDetailView{
		Number:    pr.Number,
		State:     prReviewState(pr),
		Title:     pr.Title,
		Body:      pr.Body,
		HTMLURL:   pr.HTMLURL,
		Head:      pr.HeadRef,
		Base:      pr.BaseRef,
		HeadSHA:   pr.HeadSHA,
		Author:    prAuthorView{Login: pr.AuthorLogin, AvatarURL: pr.AuthorAvatar},
		Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
	})
}

// prFileView is one row of GET .../pulls/{number}/files. Status is the single
// letter the review surface renders (A added, M modified, D deleted, R
// renamed). Patch is empty when GitHub omits it (binary or oversized file).
type prFileView struct {
	Path      string `json:"path"`
	PrevPath  string `json:"prev_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
}

// prFilesResponse is the envelope of GET .../pulls/{number}/files.
type prFilesResponse struct {
	Files []prFileView `json:"files"`
}

// prFileStatusLetter maps GitHub's file-status word to the review letter.
func prFileStatusLetter(status string) string {
	switch status {
	case "added", "copied":
		return "A"
	case "removed":
		return "D"
	case "renamed":
		return "R"
	default: // modified, changed, unchanged
		return "M"
	}
}

// handleListPRFiles returns the PR's changed files with per-file patches.
func (s *Server) handleListPRFiles(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.prContext(w, r)
	if !ok {
		return
	}
	files, err := s.GitHub.ListPRFiles(r.Context(), rc.token, rc.owner, rc.repo, rc.number)
	if !writePRFetchOK(w, err) {
		return
	}
	out := prFilesResponse{Files: make([]prFileView, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, prFileView{
			Path:      f.Filename,
			PrevPath:  f.PreviousFilename,
			Status:    prFileStatusLetter(f.Status),
			Additions: f.Additions,
			Deletions: f.Deletions,
			Patch:     f.Patch,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// prCheckRunView is one row of GET .../pulls/{number}/checks.
type prCheckRunView struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// prChecksResponse is GET .../pulls/{number}/checks: the head commit's check
// runs plus the counts the stat strip renders.
type prChecksResponse struct {
	Total   int              `json:"total"`
	Passed  int              `json:"passed"`
	Failed  int              `json:"failed"`
	Pending int              `json:"pending"`
	Runs    []prCheckRunView `json:"runs"`
}

// handleListPRChecks summarizes the check runs on the PR's head commit. It
// costs two GitHub calls (PR for the head SHA, then the runs).
func (s *Server) handleListPRChecks(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.prContext(w, r)
	if !ok {
		return
	}
	pr, err := s.GitHub.GetPR(r.Context(), rc.token, rc.owner, rc.repo, rc.number)
	if !writePRFetchOK(w, err) {
		return
	}
	runs, err := s.GitHub.ListCheckRuns(r.Context(), rc.token, rc.owner, rc.repo, pr.HeadSHA)
	if !writePRFetchOK(w, err) {
		return
	}
	out := prChecksResponse{Total: len(runs), Runs: make([]prCheckRunView, 0, len(runs))}
	for _, run := range runs {
		out.Runs = append(out.Runs, prCheckRunView{Name: run.Name, Status: run.Status, Conclusion: run.Conclusion})
		switch {
		case run.Status != "completed":
			out.Pending++
		case run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped":
			out.Passed++
		default: // failure, timed_out, cancelled, action_required, stale
			out.Failed++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// prMergeRequest is the body of POST .../pulls/{number}/merge. Method defaults
// to "merge".
type prMergeRequest struct {
	Method string `json:"method"`
}

// prMergeResponse is the 200 body of POST .../pulls/{number}/merge.
type prMergeResponse struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
}

// handleMergePR merges the PR — the review surface's one primary action. The
// refusal statuses mirror GitHub's: 422 not_mergeable (draft/conflicts/branch
// protection), 409 head_changed, 403 merge_forbidden.
func (s *Server) handleMergePR(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.prContext(w, r)
	if !ok {
		return
	}
	var req prMergeRequest
	// An empty body means the default method; malformed JSON is still a 400.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	method := req.Method
	if method == "" {
		method = "merge"
	}
	if method != "merge" && method != "squash" && method != "rebase" {
		writeError(w, http.StatusBadRequest, "bad_merge_method")
		return
	}

	res, err := s.GitHub.MergePR(r.Context(), rc.token, rc.owner, rc.repo, rc.number, method)
	if err != nil {
		switch {
		case errors.Is(err, github.ErrPRNotFound):
			writeError(w, http.StatusNotFound, "no_pr")
		case errors.Is(err, github.ErrPRNotMergeable):
			writeError(w, http.StatusUnprocessableEntity, "not_mergeable")
		case errors.Is(err, github.ErrPRHeadChanged):
			writeError(w, http.StatusConflict, "head_changed")
		case errors.Is(err, github.ErrPRMergeForbidden):
			writeError(w, http.StatusForbidden, "merge_forbidden")
		default:
			writeError(w, http.StatusBadGateway, "github_unavailable")
		}
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   rc.uid,
		Actor:    audit.UserActor(rc.uid),
		Action:   audit.ActionGitPRMerge,
		Target:   rc.owner + "/" + rc.repo,
		Metadata: map[string]any{"number": rc.number, "method": method, "sha": res.SHA},
	})
	writeJSON(w, http.StatusOK, prMergeResponse{Merged: true, SHA: res.SHA})
}

// prCommentRequest is the body of POST .../pulls/{number}/comments.
type prCommentRequest struct {
	Body string `json:"body"`
}

// prCommentResponse is the 200 body of POST .../pulls/{number}/comments.
type prCommentResponse struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// handleCommentPR posts a plain PR comment (the mobile comment sheet).
func (s *Server) handleCommentPR(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.prContext(w, r)
	if !ok {
		return
	}
	var req prCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "empty_comment")
		return
	}
	cm, err := s.GitHub.CreateIssueComment(r.Context(), rc.token, rc.owner, rc.repo, rc.number, req.Body)
	if err != nil {
		if errors.Is(err, github.ErrPRNotFound) {
			writeError(w, http.StatusNotFound, "no_pr")
			return
		}
		writeError(w, http.StatusBadGateway, "github_unavailable")
		return
	}

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   rc.uid,
		Actor:    audit.UserActor(rc.uid),
		Action:   audit.ActionGitPRComment,
		Target:   rc.owner + "/" + rc.repo,
		Metadata: map[string]any{"number": rc.number, "comment_id": cm.ID},
	})
	writeJSON(w, http.StatusOK, prCommentResponse{ID: cm.ID, HTMLURL: cm.HTMLURL})
}

// prReqContext is the resolved prelude of a PR review request.
type prReqContext struct {
	owner  string
	repo   string
	number int
	uid    string
	token  string
}

// prContext resolves the shared prelude of every PR review handler: the
// authenticated user, a shape-valid {owner}/{repo}, a positive {number}, and a
// live GitHub token. It writes the error response itself and returns ok=false
// when the request cannot proceed.
func (s *Server) prContext(w http.ResponseWriter, r *http.Request) (prReqContext, bool) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return prReqContext{}, false
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !fullNameRe.MatchString(owner + "/" + repo) {
		writeError(w, http.StatusBadRequest, "bad_full_name")
		return prReqContext{}, false
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		writeError(w, http.StatusBadRequest, "bad_number")
		return prReqContext{}, false
	}

	uid := uuidString(user.ID)
	cred, err := s.Tokens.Token(r.Context(), uid)
	if errors.Is(err, github.ErrReconnectGitHub) {
		writeError(w, http.StatusConflict, "reconnect_github")
		return prReqContext{}, false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "github_unavailable")
		return prReqContext{}, false
	}
	return prReqContext{owner: owner, repo: repo, number: number, uid: uid, token: cred.AccessToken}, true
}

// writePRFetchOK maps the shared read-path errors (missing PR, GitHub down) to
// their responses. It returns false when it wrote an error.
func writePRFetchOK(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, github.ErrPRNotFound) {
		writeError(w, http.StatusNotFound, "no_pr")
	} else {
		writeError(w, http.StatusBadGateway, "github_unavailable")
	}
	return false
}
