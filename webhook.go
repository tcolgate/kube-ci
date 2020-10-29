package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/google/go-github/v32/github"
)

func (ws *workflowSyncer) webhook(w http.ResponseWriter, r *http.Request) (int, string) {
	payload, err := github.ValidatePayload(r, ws.ghSecret)
	if err != nil {
		return http.StatusBadRequest, "request did not validate"
	}

	eventType := github.WebHookType(r)
	rawEvent, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return http.StatusBadRequest, "could not parse request"
	}

	log.Printf("webhook event of type %s", eventType)

	ctx := r.Context()

	switch event := rawEvent.(type) {

	case *github.CheckSuiteEvent:
		if event.GetCheckSuite().GetApp().GetID() != ws.appID {
			return http.StatusOK, "ignoring, wrong appID"
		}
		log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "requested", "rerequested":
			return ws.webhookCheckSuite(ctx, event)
		default:
			log.Printf("unknown checksuite action %q ignored", *event.Action)
			return http.StatusOK, "unknown checksuite action ignored"
		}

	case *github.CheckRunEvent:
		if event.GetCheckRun().GetCheckSuite().GetApp().GetID() != ws.appID {
			return http.StatusOK, "ignoring, wrong appID"
		}
		log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckRun.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "rerequested":
			ev := &github.CheckSuiteEvent{
				Org:          event.Org,
				Repo:         event.Repo,
				CheckSuite:   event.CheckRun.GetCheckSuite(),
				Installation: event.Installation,
				Action:       event.Action,
			}
			return ws.webhookCheckSuite(ctx, ev)
		case "requested_action":
			return ws.webhookCheckRunRequestAction(ctx, event)
		default:
			log.Printf("unknown checkrun action %q ignored", *event.Action)
			return http.StatusOK, "unknown checkrun action ignored"
		}

	case *github.DeploymentEvent:
		return ws.webhookDeployment(ctx, event)

	case *github.DeploymentStatusEvent:
		return ws.webhookDeploymentStatus(ctx, event)

	case *github.IssueCommentEvent:
		return ws.webhookIssueComment(ctx, event)

	default:
		return http.StatusOK, fmt.Sprintf("unknown event type %T", event)
	}
}
