package telegram

type Update struct {
	UpdateID      int64             `json:"update_id"`
	Message       *Message          `json:"message,omitempty"`
	ChatMember    *ChatMemberUpdate `json:"chat_member,omitempty"`
	CallbackQuery *CallbackQuery    `json:"callback_query,omitempty"`
}

type ChatMemberUpdate struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	Date          int64      `json:"date"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}

type ChatMember struct {
	User   User   `json:"user"`
	Status string `json:"status"`
}

type Message struct {
	MessageID      int64    `json:"message_id"`
	From           *User    `json:"from,omitempty"`
	ForwardFrom    *User    `json:"forward_from,omitempty"`
	Chat           Chat     `json:"chat"`
	Text           string   `json:"text,omitempty"`
	NewChatMembers []User   `json:"new_chat_members,omitempty"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}
