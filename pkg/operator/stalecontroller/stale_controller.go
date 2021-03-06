package stalecontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	errutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"

	"github.com/eparis/bugzilla"

	"github.com/openshift-eng/shodan/pkg/cache"
	"github.com/openshift-eng/shodan/pkg/operator/bugutil"
	"github.com/openshift-eng/shodan/pkg/operator/config"
	"github.com/openshift-eng/shodan/pkg/operator/controller"
)

var priorityTransitions = []config.Transition{
	{From: "high", To: "medium"},
	{From: "medium", To: "low"},
	{From: "unspecified", To: "low"},
}

const MinimumStaleDuration = time.Hour * 24 * 30

type StaleController struct {
	controller.ControllerContext
	config config.OperatorConfig
}

func NewStaleController(ctx controller.ControllerContext, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &StaleController{
		ControllerContext: ctx,
		config:            operatorConfig,
	}
	return factory.New().WithSync(c.sync).ResyncEvery(1*time.Hour).ToController("StaleController", recorder)
}

func (c *StaleController) handleBug(bug bugzilla.Bug) (*bugzilla.BugUpdate, error) {
	klog.Infof("#%d (S:%s, P:%s, R:%s, A:%s): %s", bug.ID, bug.Severity, bug.Priority, bug.Creator, bug.AssignedTo, bug.Summary)

	bugUpdate := bugzilla.BugUpdate{
		Whiteboard: WithKeyword(WithoutKeyword(bug.Whiteboard, "LifecycleReset"), "LifecycleStale"),
	}

	flags := []bugzilla.FlagChange{}
	flags = append(flags, bugzilla.FlagChange{
		Name:      "needinfo",
		Status:    "?",
		Requestee: bug.Creator,
	})

	bugUpdate.Priority = bugutil.DegradePriority(priorityTransitions, bug.Priority)

	// if the target bug priority is being degraded to "low", make sure to also set the blocker? flag to blocker-
	if bugUpdate.Priority == "low" && hasBlockerQuestionMark(bug) {
		flags = append(flags, bugzilla.FlagChange{
			Name:      "blocker",
			Status:    "-",
			Requestee: "eparis",
		})
	}

	bugUpdate.Flags = flags
	bugUpdate.Comment = &bugzilla.BugComment{
		Body: c.config.StaleBugComment,
	}
	return &bugUpdate, nil
}

func hasBlockerQuestionMark(bz bugzilla.Bug) bool {
	for _, f := range bz.Flags {
		if f.Name != "blocker" {
			continue
		}
		if f.Status == "?" {
			return true
		}
	}
	return false
}

var processKeywords = []string{
	"PM Score",
	"UpcomingSprint",
	"This bug will be evaluated during the next sprint and prioritized appropriately.",
	"I am working on other high priority items. I will get to this bug next sprint.",
	"This bug will be evaluated next sprint.",
	"bug is actively worked on",
}

var botCommentKeywords = []string{
	`we're marking this bug as "LifecycleStale"`,
	"The LifecycleStale keyword was removed because",
}

func (c *StaleController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)
	candidates, err := getPotentiallyStaleBugs(client, c.config)
	if err != nil {
		syncCtx.Recorder().Warningf("BuglistFailed", err.Error())
		return err
	}

	klog.V(4).Infof("Got %d potentially stale bugs.", len(candidates))

	var staleBugs []*bugzilla.Bug
	for _, bug := range candidates {
		if lastSignificantChangeAt, err := LastSignificantChangeAt(client, bug, c.config); err != nil {
			syncCtx.Recorder().Warningf("GetCachedBugComments", fmt.Errorf("skipping bug #%d: %v", bug.ID, err).Error())
			continue
		} else if lastSignificantChangeAt.Before(time.Now().Add(-MinimumStaleDuration)) {
			staleBugs = append(staleBugs, bug)
		}
	}

	var errors []error

	notifications := map[string][]string{}

	staleBugLinks := []string{}
	for _, bug := range staleBugs {
		bugUpdate, err := c.handleBug(*bug)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		if err := client.UpdateBug(bug.ID, *bugUpdate); err != nil {
			errors = append(errors, err)
			continue // don't notify on errors
		}
		// in some cases, the search query return zero assignee or creator, which cause the slack messages failed to deliver.
		// in that case, try to get the bug directly, which should populate all fields.
		if len(bug.AssignedTo) == 0 || len(bug.Creator) == 0 {
			b, err := client.GetBug(bug.ID)
			if err == nil {
				bug = b
			}
		}
		staleBugLinks = append(staleBugLinks, bugutil.FormatBugMessage(*bug))
		notifications[bug.AssignedTo] = append(notifications[bug.AssignedTo], bugutil.FormatBugMessage(*bug))
		if bug.AssignedTo != bug.Creator && bug.ID != 1801755 { // #1801755 is buggy and keeps being updated successfully (code 200), but the changes never stick.
			notifications[bug.Creator] = append(notifications[bug.Creator], bugutil.FormatBugMessage(*bug))
		}
	}

	for target, messages := range notifications {
		message := fmt.Sprintf("Hi there!\nThese bugs you are assigned to or you created were just marked as _LifecycleStale_:\n\n%s\n\nPlease review these and remove this flag if you think they are still valid bugs.",
			strings.Join(messages, "\n"))

		if err := slackClient.MessageEmail(target, message); err != nil {
			syncCtx.Recorder().Warningf("MessageFailed", fmt.Sprintf("Message to %q failed to send: %v", target, err))
		}
	}

	if len(notifications) > 0 {
		syncCtx.Recorder().Event("StaleCommentsBugs", fmt.Sprintf("Following notifications sent:\n%s\n", strings.Join(staleBugLinks, "\n")))
	}

	return errutil.NewAggregate(errors)
}

func LastSignificantChangeAt(client cache.BugzillaClient, bug *bugzilla.Bug, operatorConfig config.OperatorConfig) (time.Time, error) {
	keywords := append(append([]string{operatorConfig.StaleBugComment}, botCommentKeywords...), processKeywords...)
	return LastNonKeywordChangeAt(client, bug, keywords)
}

func LastSignificantOrBotChangeAt(client cache.BugzillaClient, bug *bugzilla.Bug) (time.Time, error) {
	return LastNonKeywordChangeAt(client, bug, processKeywords)
}

func LastNonKeywordChangeAt(client cache.BugzillaClient, bug *bugzilla.Bug, keywords []string) (time.Time, error) {
	comments, err := client.GetCachedBugComments(bug.ID, bug.LastChangeTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("GetCachedBugComments failed: %v", err)
	}

	createdAt, err := time.Parse(time.RFC3339, bug.CreationTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("creation time %q parse error: %v", bug.ID, err)
	}

	lastSignificantChangeAt := createdAt
NextComment:
	for _, cmt := range comments {
		shortText := strings.Split(cmt.Text, "\n")[0]

		for _, keyword := range keywords {
			if strings.Contains(cmt.Text, keyword) {
				klog.V(4).Infof("Ignoring comment #%d for #%d due to keyword %q: %s", cmt.Count, bug.ID, keyword, shortText)
				continue NextComment
			}
		}

		createdAt, err := time.Parse(time.RFC3339, cmt.Time)
		if err != nil {
			klog.Warningf("Skipping comment #%d of bug #%d because of time %q parse error: %v", cmt.Count, bug.ID, cmt.Time, err)
			continue
		}
		if createdAt.After(lastSignificantChangeAt) {
			lastSignificantChangeAt = createdAt
		}
	}

	history, err := client.GetCachedBugHistory(bug.ID, bug.LastChangeTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("GetCachedBugHistory failed: %v", err)
	}
NextHistory:
	for _, item := range history {
		for _, c := range item.Changes {
			if c.FieldName == "whiteboard" && strings.Contains(c.Removed, "LifecycleStale") && !strings.Contains(c.Added, "LifecycleStale") {
				changedAt, err := time.Parse(time.RFC3339, item.When)
				if err != nil {
					klog.Warningf("Skipping change on %s of bug #%d because of time %q parse error: %v", item.When, bug.ID, item.When, err)
					continue NextHistory
				}
				if changedAt.After(lastSignificantChangeAt) {
					klog.V(4).Infof("LifecycleStale removed from #%d, counting as significant change", bug.ID)
					lastSignificantChangeAt = changedAt
				}
				break
			}
		}
	}

	return lastSignificantChangeAt, nil
}

func getPotentiallyStaleBugs(client cache.BugzillaClient, c config.OperatorConfig) ([]*bugzilla.Bug, error) {
	return client.Search(bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW", "ASSIGNED", "POST", "ON_DEV"},
		Component:      c.Components.List(),
		Advanced: []bugzilla.AdvancedQuery{
			{
				Field: "status_whiteboard",
				Op:    "notsubstring",
				Value: "LifecycleStale",
			},
			{
				Negate: true,
				Field:  "external_bugzilla.description",
				Op:     "substring",
				Value:  "Customer Portal",
			},
			{
				Negate: true,
				Field:  "external_bugzilla.description",
				Op:     "substring",
				Value:  "Github",
			},
			{
				Field: "bug_severity",
				Op:    "notequals",
				Value: "urgent",
			},
			{
				Field: "short_desc",
				Op:    "notsubstring",
				Value: "CVE",
			},
			{
				Field: "keywords",
				Op:    "notsubstring",
				Value: "Security",
			},
			{
				Field: "status_whiteboard",
				Op:    "notsubstring",
				Value: "LifecycleFrozen",
			},
			{
				Field: "keywords",
				Op:    "notsubstring",
				Value: "Blocker",
			},
		},
		IncludeFields: []string{
			"id",
			"creation_time",
			"last_change_time",
			"assigned_to",
			"reporter",
			"severity",
			"priority",
			"summary",
			"whiteboard",
		},
	})
}

func WithoutKeywordAndNonEmpty(wb string, kwd string) string {
	if !strings.Contains(wb, kwd) {
		return wb
	}
	wb = WithoutKeyword(wb, kwd)
	if wb == "" {
		return " "
	}
	return wb
}

func WithoutKeyword(wb string, kwd string) string {
	var ws []string
	for _, w := range strings.Split(wb, " ") {
		if w == kwd || w == "" {
			continue
		}
		ws = append(ws, w)
	}
	return strings.Join(ws, " ")
}

func WithKeyword(wb string, kwd string) string {
	if strings.Contains(wb, kwd) {
		return wb
	}
	return strings.TrimSpace(strings.TrimSpace(wb) + " " + kwd)
}
