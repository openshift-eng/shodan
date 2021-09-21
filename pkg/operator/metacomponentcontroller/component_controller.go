package metacomponentcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eparis/bugzilla"
	"github.com/openshift-eng/shodan/pkg/operator/bugutil"
	"github.com/openshift-eng/shodan/pkg/operator/config"
	"github.com/openshift-eng/shodan/pkg/operator/controller"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
)

// MetaComponentController purpose is to search for NEW bugs that has Developer Whiteboard field populated with "meta-component" names.
// When such bug is found, and it is assigned to default component owner, this controller will reassign that bug to "meta-component" owner.
// For example, if an SingleNode keyword is found in Developer Whiteboard and the bug is NEW and assigned to Stefan, this controller will reassign
// this bug automatically to SNO engineer and move it to ASSIGNED.
type MetaComponentController struct {
	controller.ControllerContext
	config config.OperatorConfig
}

func NewMetaComponentController(ctx controller.ControllerContext, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &MetaComponentController{
		ControllerContext: ctx,
		config:            operatorConfig,
	}
	return factory.New().WithSync(c.sync).ResyncEvery(3*time.Hour).ToController("MetaComponentController", recorder)
}

func (c *MetaComponentController) sync(ctx context.Context, context factory.SyncContext) error {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	// if no config is found, fail fast
	if c.config.MetaComponents == nil {
		return nil
	}

	result, err := client.Search(getBugsQuery(c.config.Components.List(), c.config.MetaComponents.List()))
	if err != nil {
		return err
	}

	bugsToUpdate := map[int]bugzilla.BugUpdate{}

	for i := range result {
		for name, component := range c.config.MetaComponents {
			// we only looking for bugs with meta component name inside developer whiteboard
			// and bugs that does not have meta component lead already assigned to them.
			// NOTE: the search result only search for NEW bugs, so ASSIGNED or any other bugs are not present here.
			if !strings.Contains(result[i].DevelWhiteboard, name) || result[i].AssignedTo == component.Lead {
				continue
			}
			c, ok := c.config.Components[result[i].Component[0]]
			if !ok {
				continue
			}
			// we don't want to override bugs that are already assigned to meta component engineer
			// for that we only reassign bugs that are assigned to original component owner.
			if result[i].AssignedTo != c.Lead {
				continue
			}

			// ok, so we found a bug, that is in NEW state, has default component lead assigned and has meta-component
			// keyword (eg. 'SingleNode') present in developer whiteboard.
			bugsToUpdate[result[i].ID] = bugzilla.BugUpdate{
				Status:     "ASSIGNED",
				AssignedTo: component.Lead,
			}
			break
		}
	}

	tagCounter := 0
	messages := []string{}
	for bugID, update := range bugsToUpdate {
		if err := client.UpdateBug(bugID, update); err != nil {
			context.Recorder().Warningf("BugUpdateFailed", fmt.Sprintf("Failed to reassign bug %d: %v", bugID, err))
			continue
		}
		messages = append(messages, fmt.Sprintf("> Bug %s reassigned to %s", bugutil.GetBugURL(bugzilla.Bug{ID: bugID}), update.AssignedTo))
		tagCounter++
	}

	if tagCounter == 0 {
		return nil
	}

	return slackClient.MessageAdminChannel(fmt.Sprintf("%d bugs reassigned:\n\n%s", tagCounter, strings.Join(messages, "\n")))
}

func getBugsQuery(components, metaComponents []string) bugzilla.Query {
	return bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW"},
		Component:      components,
		Advanced: []bugzilla.AdvancedQuery{
			{
				Field: "cf_devel_whiteboard",
				Op:    "anywordssubstr",
				Value: strings.Join(metaComponents, ","),
			},
		},
		IncludeFields: []string{
			"id",
			"creation_time",
			"status",
			"assigned_to",
			"cf_devel_whiteboard",
		},
	}
}
