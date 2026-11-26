package ir

type LayoutSpec struct {
	SchemaVersion   string `json:"schema_version"`
	Name            string `json:"name"`
	PaperSize       string `json:"paper_size"`
	Columns         int    `json:"columns"`
	Mode            string `json:"mode"`
	ShowAnswer      bool   `json:"show_answer"`
	ShowAnalysis    bool   `json:"show_analysis"`
	ShowBindingLine bool   `json:"show_binding_line"`
	OptionLayout    string `json:"option_layout"`
	HeaderText      string `json:"header_text,omitempty"`
	FooterText      string `json:"footer_text,omitempty"`
}
