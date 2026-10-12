package cursor

import "testing"

// A chat's messages are counted as the collector's reader counts them: from
// its headers when it has them, even an empty list, and from its inline
// conversation only when it has none.
func TestComposerMessagesCountedAsTheReaderCountsThem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		counted bool
	}{
		{`{"_v":18,"composerId":"a","fullConversationHeadersOnly":[{"bubbleId":"m"}]}`, true},
		{`{"_v":18,"composerId":"a","fullConversationHeadersOnly":[],"conversation":[{"bubbleId":"m"}]}`, false},
		{`{"_v":3,"composerId":"a","conversation":[{"bubbleId":"m"}]}`, true},
	}
	for _, tc := range cases {
		c, ok := decodeComposerData("composerData:a", []byte(tc.value))
		if !ok || c.counted != tc.counted {
			t.Errorf("%s: ok %v counted %v, want %v", tc.value, ok, c.counted, tc.counted)
		}
	}
}

// A workspaceIdentifier in any shape is recorded, even when it names no local
// folder or ID, so the chat is not mistaken for one without folder evidence.
func TestComposerRecordsWorkspaceIdentifierPresence(t *testing.T) {
	t.Parallel()
	const head = `{"_v":18,"composerId":"a","fullConversationHeadersOnly":[{"bubbleId":"m"}]`
	cases := []struct {
		name    string
		extra   string
		folder  string
		present bool
	}{
		{name: "absent", extra: ``, folder: "", present: false},
		{name: "null", extra: `,"workspaceIdentifier":null`, folder: "", present: false},
		{name: "local", extra: `,"workspaceIdentifier":{"uri":"file:///work/site"}`, folder: "/work/site", present: true},
		{name: "remote_uri_object", extra: `,"workspaceIdentifier":{"uri":{"scheme":"vscode-remote","path":"/srv/x"}}`, folder: "", present: true},
		{name: "remote_uri_string", extra: `,"workspaceIdentifier":{"uri":"vscode-remote://ssh-remote+h/srv/x"}`, folder: "", present: true},
		{name: "unknown_uri_shape", extra: `,"workspaceIdentifier":{"uri":7}`, folder: "", present: true},
		{name: "unknown_shape", extra: `,"workspaceIdentifier":"x"`, folder: "", present: true},
		{name: "empty_object", extra: `,"workspaceIdentifier":{}`, folder: "", present: true},
	}
	for _, tc := range cases {
		c, ok := decodeComposerData("composerData:a", []byte(head+tc.extra+`}`))
		if !ok || c.chat.Folder != tc.folder || c.chat.WorkspaceIdentifier != tc.present || c.chat.WorkspaceID != "" {
			t.Errorf("%s: ok %v chat %+v, want folder %q present %v", tc.name, ok, c.chat, tc.folder, tc.present)
		}
	}
}
