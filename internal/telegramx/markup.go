// Package telegramx — расширения к моделям go-telegram/bot для полей Bot API, которых ещё нет в models.
package telegramx

import "github.com/go-telegram/bot/models"

// InlineKeyboardMarkup совместим с Telegram Bot API (reply_markup).
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton — как models.InlineKeyboardButton, плюс icon_custom_emoji_id (Bot API 7+).
// Иконка показывается перед текстом кнопки (не заменяет текст).
// См. https://core.telegram.org/bots/api#inlinekeyboardbutton
type InlineKeyboardButton struct {
	Text                         string                       `json:"text"`
	IconCustomEmojiID            string                       `json:"icon_custom_emoji_id,omitempty"`
	URL                          string                       `json:"url,omitempty"`
	CallbackData                 string                       `json:"callback_data,omitempty"`
	WebApp                       *models.WebAppInfo           `json:"web_app,omitempty"`
	LoginURL                     *models.LoginURL             `json:"login_url,omitempty"`
	SwitchInlineQuery            string                       `json:"switch_inline_query,omitempty"`
	SwitchInlineQueryCurrentChat string                       `json:"switch_inline_query_current_chat,omitempty"`
	SwitchInlineQueryChosenChat  *models.SwitchInlineQueryChosenChat `json:"switch_inline_query_chosen_chat,omitempty"`
	CopyText                     models.CopyTextButton        `json:"copy_text,omitempty"`
	CallbackGame                 *models.CallbackGame         `json:"callback_game,omitempty"`
	Pay                          bool                         `json:"pay,omitempty"`
}
