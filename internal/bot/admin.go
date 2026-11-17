package bot

import (
	"fmt"
	"html"
	"strings"
)

// onAdminCommand handles the commands of the users listed in ADMIN_IDS.
// It reports false for a command it does not know.
func (b *Bot) onAdminCommand(chatID int64, name, arg string) bool {
	userList := map[string]struct {
		apply func(int64) error
		done  string
	}{
		"allow":    {func(id int64) error { return b.store.SetAllowed(id, true) }, "Пользователь %d добавлен в белый список. Теперь бот отвечает только тем, кто в списке."},
		"disallow": {func(id int64) error { return b.store.SetAllowed(id, false) }, "Пользователь %d убран из белого списка."},
		"ban":      {func(id int64) error { return b.store.SetDenied(id, true) }, "Пользователь %d заблокирован."},
		"unban":    {func(id int64) error { return b.store.SetDenied(id, false) }, "Пользователь %d разблокирован."},
	}

	switch {
	case name == "stats":
		b.send(chatID, b.textStats(), nil)

	case userList[name].apply != nil:
		id, ok := parseID(arg)
		if !ok {
			b.send(chatID, strings.ReplaceAll(textAdminUsage, "%s", name), nil)
			return true
		}
		if err := userList[name].apply(id); err != nil {
			b.warnStore(err)
			b.send(chatID, textInternal, nil)
			return true
		}
		b.send(chatID, fmt.Sprintf(userList[name].done, id), nil)

	case name == "blockvideo" || name == "unblockvideo":
		// Both a bare video ID and a link are accepted.
		videoID := arg
		if link, _, _, err := b.findLink(arg); err == nil {
			videoID = link.ID
		}
		if videoID == "" || strings.ContainsAny(videoID, " /") {
			b.send(chatID, strings.ReplaceAll(textAdminUsage, "%s", name), nil)
			return true
		}
		block := name == "blockvideo"
		shown := html.EscapeString(videoID)
		if err := b.store.SetVideoBlocked(videoID, block); err != nil {
			b.warnStore(err)
			b.send(chatID, textInternal, nil)
			return true
		}
		if block {
			b.send(chatID, fmt.Sprintf("Видео <code>%s</code> закрыто для скачивания.", shown), nil)
		} else {
			b.send(chatID, fmt.Sprintf("Видео <code>%s</code> снова доступно.", shown), nil)
		}

	default:
		return false
	}
	return true
}
