package backlog

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
)

// BoardStatusMap is the whole of what this tracker adapter knows about the
// board: which Backlog status carries each of the engine's four phases. The
// engine speaks phases; these identifiers exist only here and in the
// deployment's configuration, so replacing the tracker replaces this file and
// nothing upstream.
type BoardStatusMap struct {
	Running        int64
	AwaitingAnswer int64
	Delivered      int64
	NeedsAttention int64
}

func (m BoardStatusMap) validate() error {
	if m.Running <= 0 || m.AwaitingAnswer <= 0 || m.Delivered <= 0 || m.NeedsAttention <= 0 {
		return errors.New("board status map is incomplete")
	}
	return nil
}

func (m BoardStatusMap) statusFor(phase hook.BoardPhase) (int64, bool) {
	switch phase {
	case hook.BoardRunning:
		return m.Running, true
	case hook.BoardAwaitingAnswer:
		return m.AwaitingAnswer, true
	case hook.BoardDelivered:
		return m.Delivered, true
	case hook.BoardNeedsAttention:
		return m.NeedsAttention, true
	default:
		return 0, false
	}
}

// BoardProjection moves a Backlog issue between statuses to mirror the
// engine's phase. It is a view of the run state, never an input to it.
type BoardProjection struct {
	client   *Client
	statuses BoardStatusMap
}

func NewBoardProjection(client *Client, statuses BoardStatusMap) (*BoardProjection, error) {
	if client == nil {
		return nil, errors.New("board projection needs a client")
	}
	if err := statuses.validate(); err != nil {
		return nil, err
	}
	return &BoardProjection{client: client, statuses: statuses}, nil
}

func (p *BoardProjection) ProjectBoardPhase(ctx context.Context, issueID int64, phase hook.BoardPhase) error {
	statusID, known := p.statuses.statusFor(phase)
	if !known {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "unknown_board_phase")
	}
	return p.client.UpdateIssueStatus(ctx, issueID, statusID)
}

// UpdateIssueStatus moves the issue to the given status. The response is
// checked to carry the requested status, so a silently ignored update cannot
// read as success.
func (c *Client) UpdateIssueStatus(ctx context.Context, issueID, statusID int64) error {
	if issueID <= 0 || statusID <= 0 {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_status_update")
	}
	endpoint := *c.origin
	endpoint.Path = "/api/v2/issues/" + strconv.FormatInt(issueID, 10)
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	endpoint.RawQuery = query.Encode()
	form := url.Values{"statusId": []string{strconv.FormatInt(statusID, 10)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var updated struct {
		ID     int64 `json:"id"`
		Status struct {
			ID int64 `json:"id"`
		} `json:"status"`
	}
	if err := c.doJSON(request, http.StatusOK, &updated); err != nil {
		return err
	}
	if updated.ID != issueID || updated.Status.ID != statusID {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return nil
}
