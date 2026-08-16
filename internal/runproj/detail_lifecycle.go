package runproj

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runstatus"
)

// RunOwnerLifecycle reports the workflow root's ownership boundary.
type RunOwnerLifecycle struct {
	Status             string `json:"status"`
	RootStatus         string `json:"rootStatus"`
	Owned              bool   `json:"owned"`
	AwaitingOwnerClose bool   `json:"awaitingOwnerClose"`
}

// RunWorkflowControl reports orchestrator-control progress independently of the root.
type RunWorkflowControl struct {
	Status  string `json:"status"`
	Total   int    `json:"total"`
	Open    int    `json:"open"`
	Closed  int    `json:"closed"`
	Blocked int    `json:"blocked"`
	Failed  int    `json:"failed"`
}

// RunDelivery reports implementation and final-report evidence.
type RunDelivery struct {
	Status                    string `json:"status"`
	Outcome                   string `json:"outcome,omitempty"`
	ImplementationSummaryPath string `json:"implementationSummaryPath,omitempty"`
	FinalReportPath           string `json:"finalReportPath,omitempty"`
}

// RunPublish reports the pack-authored publish result without implying merge.
type RunPublish struct {
	Status            string `json:"status"`
	Action            string `json:"action,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ArtifactPath      string `json:"artifactPath,omitempty"`
	RemoteStatus      string `json:"remoteStatus,omitempty"`
	PublishedCommit   string `json:"publishedCommit,omitempty"`
	PullRequestURL    string `json:"pullRequestUrl,omitempty"`
	PullRequestNumber string `json:"pullRequestNumber,omitempty"`
}

// RunMerge reports merge or pull-request evidence independently of publish.
type RunMerge struct {
	Status         string `json:"status"`
	Commit         string `json:"commit,omitempty"`
	Target         string `json:"target,omitempty"`
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
}

type runLifecycleProjection struct {
	owner    RunOwnerLifecycle
	control  RunWorkflowControl
	delivery RunDelivery
	publish  RunPublish
	merge    RunMerge
}

func projectRunLifecycle(root beads.Bead, members []beads.Bead) runLifecycleProjection {
	status := runstatus.Build(root, members)
	return runLifecycleProjection{
		owner: RunOwnerLifecycle{
			Status:             status.Owner.Status,
			RootStatus:         status.Owner.RootStatus,
			Owned:              status.Owner.Owned,
			AwaitingOwnerClose: status.Owner.AwaitingOwnerClose,
		},
		control: RunWorkflowControl{
			Status:  status.Controls.Status,
			Total:   status.Controls.Total,
			Open:    status.Controls.Open,
			Closed:  status.Controls.Closed,
			Blocked: status.Controls.Blocked,
			Failed:  status.Controls.Failed,
		},
		delivery: RunDelivery{
			Status:                    status.Delivery.Status,
			Outcome:                   status.Delivery.Outcome,
			ImplementationSummaryPath: status.Delivery.ImplementationSummaryPath,
			FinalReportPath:           status.Delivery.FinalReportPath,
		},
		publish: RunPublish{
			Status:            status.Publish.Status,
			Action:            status.Publish.Action,
			Reason:            status.Publish.Reason,
			ArtifactPath:      status.Publish.ArtifactPath,
			RemoteStatus:      status.Publish.RemoteStatus,
			PublishedCommit:   status.Publish.PublishedCommit,
			PullRequestURL:    status.Publish.PullRequestURL,
			PullRequestNumber: status.Publish.PullRequestNumber,
		},
		merge: RunMerge{
			Status:         status.Merge.Status,
			Commit:         status.Merge.Commit,
			Target:         status.Merge.Target,
			PullRequestURL: status.Merge.PullRequestURL,
		},
	}
}
