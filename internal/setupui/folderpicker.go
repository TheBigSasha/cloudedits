package setupui

import "context"

// FolderPickResult describes the outcome of a native OS folder picker request.
type FolderPickResult struct {
	Supported bool   `json:"supported"`
	Cancelled bool   `json:"cancelled"`
	Path      string `json:"path,omitempty"`
}

// pickFolderFunc invokes the platform-native folder picker (overridable in tests).
var pickFolderFunc = pickFolderNative

func pickFolder(ctx context.Context) (FolderPickResult, error) {
	return pickFolderFunc(ctx)
}
