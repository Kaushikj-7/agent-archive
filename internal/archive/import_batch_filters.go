package archive

// ImportBatchFilters is the existing local batch filter wire, shared with
// historical authority checks. It does not derive or grant source permission.
type ImportBatchFilters struct {
	MappingDigest  string   `json:"mapping_digest,omitempty"`
	Harnesses      []string `json:"harnesses"`
	ProjectIDs     []string `json:"project_ids"`
	Since          string   `json:"since,omitempty"`
	Until          string   `json:"until,omitempty"`
	SinceArg       string   `json:"since_arg,omitempty"`
	UntilArg       string   `json:"until_arg,omitempty"`
	IncludeHome    bool     `json:"include_home"`
	IncludeTemp    bool     `json:"include_temp"`
	IncludeRemoved bool     `json:"include_removed"`
}
