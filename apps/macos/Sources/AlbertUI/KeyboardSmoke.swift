import AppKit
import AlbertCore

/// Runs inside the actual app binary without credentials or a server.
@MainActor
public enum KeyboardSmoke {
    public static func event(code: UInt16, characters: String, modifiers: NSEvent.ModifierFlags = .command, repeatKey: Bool = false, window: NSWindow? = nil) -> NSEvent {
        NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: modifiers, timestamp: 0, windowNumber: window?.windowNumber ?? 0, context: nil, characters: characters, charactersIgnoringModifiers: characters, isARepeat: repeatKey, keyCode: code)!
    }
    public static func run(controller: ApplicationController) -> Bool {
        let model = controller.model
        model.suppressNetwork = true
        model.showSettings = false
        model.draft.text = "Черновик теста"
        controller.window.layoutIfNeeded()
        // SwiftUI lays out its native editor on the main run loop.
        RunLoop.current.run(until: Date().addingTimeInterval(0.1))
        guard let editor = model.composer else { return false }
        controller.window.makeFirstResponder(editor)
        var sent = 0
        let original = editor.onSend
        editor.onSend = { sent += 1 }
        let enter = event(code: 36, characters: "\r", window: controller.window)
        editor.keyDown(with: event(code: 36, characters: "\r", modifiers: []))
        guard sent == 1 else { return false }
        editor.keyDown(with: event(code: 36, characters: "\r", modifiers: [], repeatKey: true))
        guard sent == 1 else { return false }
        // Physical Return works regardless of produced characters/layout.
        editor.keyDown(with: event(code: 36, characters: "Ж", modifiers: []))
        guard sent == 2 else { return false }
        editor.setMarkedText("に", selectedRange: NSRange(location: 1, length: 0), replacementRange: NSRange(location: NSNotFound, length: 0))
        _ = editor.performKeyEquivalent(with: enter)
        guard editor.hasMarkedText(), sent == 2 else { return false }
        editor.unmarkText()
        editor.onSend = original
        model.draft.text = "Сохранённый черновик"
        let menu = NSApp.mainMenu!
        guard menu.performKeyEquivalent(with: event(code: 19, characters: "2")), model.section == .tasks else { return false }
        guard menu.performKeyEquivalent(with: event(code: 18, characters: "1")), model.section == .conversation else { return false }
        guard model.draft.text == "Сохранённый черновик" else { return false }
        guard menu.performKeyEquivalent(with: event(code: 42, characters: "\\")), model.sidebarVisible else { return false }
        model.escape()
        guard !model.sidebarVisible else { return false }
        // Click and keyboard shortcut share one action, including after the
        // toolbar button takes focus. Neither should consume the text draft.
        guard let sections = controller.window.toolbar?.items.first(where: { $0.itemIdentifier.rawValue == "sections" })?.view as? NSButton else { return false }
        controller.window.makeFirstResponder(sections)
        sections.performClick(nil)
        guard model.sidebarVisible, model.draft.text == "Сохранённый черновик" else { return false }
        guard menu.performKeyEquivalent(with: event(code: 42, characters: "\\")), !model.sidebarVisible else { return false }
        controller.window.makeFirstResponder(editor)
        guard let primary = controller.commands["primary"], !controller.validateMenuItem(primary) else { return false }
        model.draft = Draft()
        return true
    }
}

@MainActor
extension KeyboardSmoke {
    public static func snapshots(controller: ApplicationController, directory: URL, imagePath: String?) throws {
        let model = controller.model
        model.suppressNetwork = true
        model.sidebarVisible = false
        model.showSettings = false
        model.section = .conversation
        model.draft = Draft()
        let file = Attachment(id: "smoke-image", name: "Озеро.jpg", mime: "image/jpeg", sizeBytes: 173245)
        var attachments: [Attachment] = []
        if let imagePath, let image = NSImage(contentsOfFile: imagePath) { model.images[file.id] = image; attachments = [file] }
        model.content.messages = [
            Message(id: "snapshot-user", role: "user", text: "Собери фотографии в заметку. Добавь порядок публикации и команду экспорта."),
            Message(id: "snapshot-assistant", role: "assistant", text: "## Озеро и лес\n\nДва кадра для одной заметки: **общий план** и *лесная тропа*.\n\n### Порядок публикации\n\n1. Начать с озера: оно задаёт масштаб.\n2. Следом поставить тропу — более близкий кадр.\n\n> Оригиналы сохранить отдельно.\n\n### Экспорт\n\n```sh\nmkdir -p export\nmagick lake.jpg -resize '1600x>' export/lake.jpg\n```", format: "markdown", delivery: "sent", attachments: attachments),
        ]
        model.error = nil
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        // Larger snapshot shows the complete example without changing the user's window.
        controller.window.setContentSize(NSSize(width: 900, height: 1080))
        try snapshot(controller.window, to: directory.appendingPathComponent("conversation.png"))
        let taskJSON = Data(#"[{"id":"desktop","title":"Подготовить приложение для macOS","goal":"Подготовить первую версию: единый разговор, поручения, память и файлы.","status":"waiting","reason":"Выбрать версию macOS","version":1},{"id":"availability","title":"Проверять доступность Альберта","goal":"Сообщать о сбоях","status":"running","reason":"Следующая проверка через 5 минут","version":1},{"id":"photos","title":"Собрать фотографии в заметку","goal":"Подготовить заметку","status":"completed","result":"Заметка готова.","version":1}]"#.utf8)
        model.content.tasks = try APIJSON.decoder().decode([AlbertTask].self, from: taskJSON)
        model.section = .tasks; model.selectedTaskID = nil
        controller.window.setContentSize(NSSize(width: 900, height: 720))
        try snapshot(controller.window, to: directory.appendingPathComponent("tasks.png"))
        model.selectedTaskID = "desktop"
        try snapshot(controller.window, to: directory.appendingPathComponent("task-detail.png"))
    }
    private static func snapshot(_ window: NSWindow, to url: URL) throws {
        RunLoop.current.run(until: Date().addingTimeInterval(0.2))
        guard let view = window.contentView else { throw APIError.invalidResponse }
        view.layoutSubtreeIfNeeded(); view.displayIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { throw APIError.invalidResponse }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        guard let data = bitmap.representation(using: .png, properties: [:]) else { throw APIError.invalidResponse }
        try data.write(to: url)
    }
}
