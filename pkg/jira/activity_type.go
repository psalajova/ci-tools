package jira

// ActivityTypeCustomFieldKey is the Activity Type custom field key (customfield_10464).
const ActivityTypeCustomFieldKey = "customfield_10464"

// AllowedActivityTypes are valid Activity Type values; labels must match Jira.
var AllowedActivityTypes = []string{
	"Associate Wellness & Development",
	"Future Sustainability",
	"Quality / Stability / Reliability",
	"Product / Portfolio Work",
	"Security & Compliance",
	"Incidents & Support",
}

// IsValidActivityType reports whether s is in AllowedActivityTypes.
func IsValidActivityType(s string) bool {
	if s == "" {
		return false
	}
	for _, v := range AllowedActivityTypes {
		if s == v {
			return true
		}
	}
	return false
}
