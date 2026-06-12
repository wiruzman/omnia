package progress

type PathProgress struct {
	Root           string  `json:"root"`
	Scanned        int64   `json:"scanned"`
	EstimatedTotal int64   `json:"estimated_total"`
	Percent        float64 `json:"percent"`
	CurrentPath    string  `json:"current_path"`
}

func ClampPercent(scanned, estimatedTotal int64) float64 {
	if scanned <= 0 || estimatedTotal <= 0 {
		return 0
	}
	p := (float64(scanned) / float64(estimatedTotal)) * 100
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}
