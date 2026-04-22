package modals

import (
	"github.com/slack-go/slack"
)

// BlockIDActivityType is the Slack block id for Activity Type.
const BlockIDActivityType = "activity_type"

// Values must match pkg/jira.AllowedActivityTypes / Jira options.
const (
	ActivityTypeAssociateWellness           = "Associate Wellness & Development"
	ActivityTypeFutureSustainability        = "Future Sustainability"
	ActivityTypeIncidentsSupport            = "Incidents & Support"
	ActivityTypeQualityStabilityReliability = "Quality / Stability / Reliability"
	ActivityTypeSecurityCompliance          = "Security & Compliance"
	ActivityTypeProductPortfolioWork        = "Product / Portfolio Work"
)

func activityTypeSelectOptions() []*slack.OptionBlockObject {
	opts := []string{
		ActivityTypeAssociateWellness,
		ActivityTypeFutureSustainability,
		ActivityTypeQualityStabilityReliability,
		ActivityTypeProductPortfolioWork,
		ActivityTypeSecurityCompliance,
		ActivityTypeIncidentsSupport,
	}
	out := make([]*slack.OptionBlockObject, 0, len(opts))
	for _, o := range opts {
		v := o
		out = append(out, &slack.OptionBlockObject{
			Text:  &slack.TextBlockObject{Type: slack.PlainTextType, Text: v},
			Value: v,
		})
	}
	return out
}

// ActivityTypeInputBlock builds an optional Activity Type static select.
func ActivityTypeInputBlock(initial string) *slack.InputBlock {
	el := &slack.SelectBlockElement{
		Type:        slack.OptTypeStatic,
		Placeholder: &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Select Activity Type"},
		Options:     activityTypeSelectOptions(),
	}
	if initial != "" {
		el.InitialOption = &slack.OptionBlockObject{
			Text:  &slack.TextBlockObject{Type: slack.PlainTextType, Text: initial},
			Value: initial,
		}
	}
	return &slack.InputBlock{
		Type:     slack.MBTInput,
		BlockID:  BlockIDActivityType,
		Optional: true,
		Label:    &slack.TextBlockObject{Type: slack.PlainTextType, Text: "Activity Type"},
		Element:  el,
	}
}
