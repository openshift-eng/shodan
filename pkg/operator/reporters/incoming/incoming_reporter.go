package incoming

import (
	"context"
	"fmt"
	"strings"

	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift-eng/shodan/pkg/cache"
	"github.com/openshift-eng/shodan/pkg/operator/bugutil"
	"github.com/openshift-eng/shodan/pkg/operator/config"
	"github.com/openshift-eng/shodan/pkg/operator/controller"
)

// IncomingReport reports bugs that are NEW and haven't been assigned yet.
// To track new bugs, we chose to tag bugs we have seen with 'AssigneeNotified' keyword (in DevWhiteboard).
// This reported will notify assignees about new bugs based on the reporter schedule (2x a day).
// Additionally, a report of new bugs will be sent to the status channel.
type IncomingReporter struct {
	controller.ControllerContext
	config     config.OperatorConfig
	components []string
}

func (c *IncomingReporter) sync(ctx context.Context, syncContext factory.SyncContext) error {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	channelReport, assigneeReports, bugs, err := Report(ctx, client, syncContext.Recorder(), c.components)
	if err != nil {
		return err
	}

	if err := updateIncomingReport(c.ControllerContext, bugs); err != nil {
		syncContext.Recorder().Warningf("IncomingReporterFailed", "Error updating bug incoming rate report: %v", err)
		return err
	}

	if len(assigneeReports) == 0 {
		return nil
	}

	// In 95% cases this will hit the default component assignees.
	for assignee, bugs := range assigneeReports {
		message := fmt.Sprintf("%s\n\n> Please set severity/priority on the bug(s) above and assign to a team member.\n", strings.Join(bugs.reports, "\n"))
		if err := slackClient.MessageEmail(assignee, message); err != nil {
			syncContext.Recorder().Warningf("DeliveryFailed", "Failed to deliver:\n\n%s\n\n to %q: %v", message, assignee, err)
			continue
		}
		for _, id := range bugs.bugIDs {
			if err := c.markAsReported(client, id); err != nil {
				syncContext.Recorder().Warningf("MarkNotifiedFailed", "Failed to mark bug #%d with AssigneeNotified: %v", id, err)
			}
		}
	}

	if err := slackClient.MessageChannel(channelReport); err != nil {
		syncContext.Recorder().Warningf("DeliveryFailed", "Failed to deliver new bugs: %v", err)
		return err
	}

	return nil
}

func NewIncomingReporter(ctx controller.ControllerContext, components []string, schedule []string, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &IncomingReporter{
		ControllerContext: ctx,
		config:            operatorConfig,
		components:        components,
	}
	return factory.New().WithSync(c.sync).ResyncSchedule(schedule...).ToController("IncomingReporter", recorder)
}

type AssigneeReport struct {
	bugIDs  []int
	reports []string
}

func Report(ctx context.Context, client cache.BugzillaClient, recorder events.Recorder, components []string) (string, map[string]AssigneeReport, []*bugzilla.Bug, error) {
	incomingBugs, err := getIncomingBugsList(client, components)
	if err != nil {
		recorder.Warningf("BugSearchFailed", err.Error())
		return "", nil, nil, err
	}

	var channelReport []string
	assigneeReports := map[string]AssigneeReport{}

	for _, bug := range incomingBugs {
		bugMessage := bugutil.FormatBugMessage(*bug)
		channelReport = append(channelReport, fmt.Sprintf("> %s", bugMessage))
		currentReport, ok := assigneeReports[bug.AssignedTo]
		if ok {
			newReport := AssigneeReport{
				reports: append(currentReport.reports, bugMessage),
				bugIDs:  append(currentReport.bugIDs, bug.ID),
			}
			assigneeReports[bug.AssignedTo] = newReport
			continue
		}
		assigneeReports[bug.AssignedTo] = AssigneeReport{
			bugIDs:  []int{bug.ID},
			reports: []string{bugMessage},
		}
	}

	return strings.Join(channelReport, "\n"), assigneeReports, incomingBugs, nil
}

func (c *IncomingReporter) markAsReported(client cache.BugzillaClient, id int) error {
	return client.UpdateBug(id, bugzilla.BugUpdate{DevWhiteboard: "AssigneeNotified", MinorUpdate: true})
}

func getIncomingBugsList(client cache.BugzillaClient, components []string) ([]*bugzilla.Bug, error) {
	return client.Search(bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW", "ASSIGNED"},
		Component:      components,
		Advanced: []bugzilla.AdvancedQuery{
			{
				Field: "cf_devel_whiteboard",
				Op:    "notsubstring",
				Value: "AssigneeNotified",
			},
		},
		IncludeFields: []string{
			"id",
			"assigned_to",
			"keywords",
			"status",
			"component",
			"resolution",
			"summary",
			"severity",
			"priority",
			"target_release",
			"whiteboard",
		},
	})
}
