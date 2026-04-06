package assets

import _ "embed"

// BotButtonsJSON встроенный assets/bot_buttons.json (Docker без копирования папки).
//
//go:embed bot_buttons.json
var BotButtonsJSON []byte
