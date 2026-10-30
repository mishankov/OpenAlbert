package domain

// GroupSettings contains only public behavior choices, never private prose.
// Empty fields inherit from the less specific scope.
type GroupSettings struct {
	ReplyMode string `json:"reply_mode,omitempty"`
	Tone      string `json:"tone,omitempty"`
	Verbosity string `json:"verbosity,omitempty"`
}

// A zero target selects all groups. A group without ConversationID selects all
// its topics; ConversationID selects one known conversation within that group.
type GroupSettingsTarget struct {
	GroupID        int64
	ConversationID string
}

type GroupSettingsPatch struct {
	ReplyMode *string `json:"reply_mode,omitempty"`
	Tone      *string `json:"tone,omitempty"`
	Verbosity *string `json:"verbosity,omitempty"`
	Reset     bool    `json:"reset,omitempty"`
}

type GroupSettingsView struct {
	Defaults  GroupSettings `json:"defaults"`
	Group     GroupSettings `json:"group"`
	Topic     GroupSettings `json:"topic"`
	Effective GroupSettings `json:"effective"`
}
