package config

type processAttrs struct {
	metadata  map[string]string
	podLabels map[string]string
}

func matchByAttributes(actual *processAttrs, required *Attributes) bool {
	if required == nil {
		return true
	}
	if actual == nil {
		return false
	}

	// match metadata
	for attrName, criteriaRegexp := range required.Metadata {
		if attrValue, ok := actual.metadata[attrName]; !ok || !criteriaRegexp.MatchString(attrValue) {
			return false
		}
	}

	// match pod labels
	for labelName, criteriaRegexp := range required.PodLabels {
		if actualPodLabelValue, ok := actual.podLabels[labelName]; !ok || !criteriaRegexp.MatchString(actualPodLabelValue) {
			return false
		}
	}
	return true
}
