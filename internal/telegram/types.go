package telegram

type Update struct {
	UpdateID        int64             `json:"update_id"`
	Message         *Message          `json:"message,omitempty"`
	ChatMember      *ChatMemberUpdate `json:"chat_member,omitempty"`
	ChatJoinRequest *ChatJoinRequest  `json:"chat_join_request,omitempty"`
	CallbackQuery   *CallbackQuery    `json:"callback_query,omitempty"`
}

// ChatJoinRequest — заявка на вступление в группу с включенным режимом заявок.
type ChatJoinRequest struct {
	Chat Chat `json:"chat"`
	From User `json:"from"`
	// UserChatID — приватный чат заявителя. Пока заявка не обработана,
	// боту разрешено писать в него даже без нажатого Start.
	UserChatID int64           `json:"user_chat_id"`
	Date       int64           `json:"date"`
	Bio        string          `json:"bio,omitempty"`
	InviteLink *ChatInviteLink `json:"invite_link,omitempty"`
}

type ChatInviteLink struct {
	InviteLink string `json:"invite_link"`
	Name       string `json:"name,omitempty"`
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
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
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
