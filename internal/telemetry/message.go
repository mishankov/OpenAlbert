package telemetry

import (
	"fmt"
	"strings"
	"time"
)

func readableMessage(operation, result string, elapsed time.Duration, fields map[string]any, diagnostic Diagnostic) string {
	labels := map[string]string{
		"application.started": "Jarvis запущен",
		"telegram.receive":    "Получено сообщение из Telegram",
		"turn.prepare":        "Подготовлен контекст обращения",
		"llm.complete":        "Получен ответ модели",
		"turn.finish":         "Сохранено состояние обращения",
		"turn.complete":       "Обращение обработано",
		"telegram.deliver":    "Ответ отправлен в Telegram",
		"browser.start":       "Запущен Chromium",
		"browser.close":       "Закрыт Chromium, слот освобождён",
		"session.analyze":     "Проанализирована завершённая сессия",
	}
	action := labels[operation]
	if operation == "tool.execute" {
		tools := map[string]string{
			"logs_search": "Прочитаны операционные логи",
			"web_read":    "Прочитана веб-страница", "web_search": "Выполнен поиск в интернете",
			"browser_open": "Открыта страница в Chromium", "browser_navigate": "Выполнен переход в Chromium",
			"browser_snapshot": "Получено содержимое страницы", "browser_click": "Выполнено нажатие на странице",
			"browser_fill": "Заполнено поле на странице", "browser_wait": "Завершено ожидание страницы",
			"browser_tabs": "Получен список вкладок", "browser_screenshot": "Сохранён скриншот страницы",
			"browser_download": "Скачан файл", "browser_close_page": "Закрыта вкладка", "browser_close": "Закрыт браузер",
			"send_message": "Ответ поставлен в очередь Telegram", "send_group_message": "Сообщение поставлено в очередь Telegram для группы", "memory_update": "Обновлена память",
			"memory_search": "Выполнен поиск по памяти", "memory_read": "Прочитана запись памяти",
			"task_create": "Создана длительная задача", "task_find": "Найдены задачи", "task_read": "Прочитано состояние задачи", "task_update": "Обновлена задача",
			"artifact_save": "Сохранён артефакт", "artifact_read": "Прочитан артефакт",
			"conversation_list": "Получен список доступных разговоров", "conversation_history": "Прочитана история разговора",
			"participant_search": "Найдены участники группы", "participant_read": "Прочитан профиль участника", "participant_update": "Обновлён профиль участника",
			"soul_read": "Прочитан SOUL.md", "soul_update": "Обновлён характер в SOUL.md",
			"skill_read": "Прочитан навык", "skill_propose_change": "Сохранено предложение изменения навыка",
		}
		name, _ := fields["tool"].(string)
		action = tools[name]
		if action == "" {
			action = "Вызван инструмент " + name
		}
		if name == "memory_update" && fields["memory_kind"] == "profile" {
			action = "Обновлён профиль пользователя"
		}
	}
	if action == "" {
		action = operation
	}
	if result == "error" {
		action = "Ошибка операции " + operation
		if operation == "tool.execute" {
			name, _ := fields["tool"].(string)
			action = "Ошибка инструмента " + name
			switch name {
			case "web_read":
				action = "Ошибка чтения веб-страницы"
			case "logs_search":
				action = "Ошибка чтения операционных логов"
			case "browser_open", "browser_navigate":
				action = "Ошибка открытия страницы в Chromium"
			case "browser_click":
				action = "Ошибка нажатия на странице"
			case "send_message":
				action = "Ошибка постановки ответа в очередь"
			case "memory_update":
				action = "Ошибка обновления памяти"
			}
		} else if operation == "llm.complete" {
			action = "Ошибка вызова модели"
		} else if operation == "turn.complete" {
			action = "Обращение завершилось ошибкой"
		}
	} else if result == "cancelled" {
		action = "Операция отменена"
	} else if result == "waiting" {
		action = "Ожидание свободного слота Chromium"
	}
	if operation == "telegram.deliver" && fields["delivery_state"] != "sent" {
		action = "Ответ в Telegram не доставлен"
	}
	parts := []string{action}
	if host, ok := fields["destination_host"].(string); ok && host != "" {
		parts = append(parts, host)
	}
	if model, ok := fields["model"].(string); ok && model != "" {
		parts = append(parts, model)
	}
	_, durationKnown := fields["processing_ms"]
	if operation == "turn.complete" && !durationKnown {
		parts = append(parts, "длительность неизвестна")
	} else if elapsed < time.Millisecond {
		parts = append(parts, "<1 мс")
	} else if elapsed < time.Second {
		parts = append(parts, fmt.Sprintf("%d мс", elapsed.Milliseconds()))
	} else {
		parts = append(parts, fmt.Sprintf("%.1f с", elapsed.Seconds()))
	}
	if operation == "turn.complete" {
		parts = append(parts, fmt.Sprintf("вызовов модели: %v, инструментов: %v", fields["model_calls"], fields["tool_calls"]))
	}
	if input, ok := fields["input_tokens"]; ok {
		parts = append(parts, fmt.Sprintf("токены: %v вход / %v выход", input, fields["output_tokens"]))
	}
	if operation == "turn.complete" && fields["delivery_state"] == "queued" {
		parts = append(parts, "ответ в очереди Telegram")
	}
	if fields["cached"] == true {
		parts = append(parts, "сохранённый результат, без повторного выполнения")
	}
	if result == "error" {
		parts = append(parts, diagnostic.Code)
	}
	if id, ok := fields["turn_id"].(string); ok && id != "" {
		if len(id) > 8 {
			id = id[:8]
		}
		parts = append(parts, "обращение "+id)
	}
	return strings.Join(parts, " · ")
}
