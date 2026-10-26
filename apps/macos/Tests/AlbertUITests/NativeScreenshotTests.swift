import XCTest
import AppKit
import AlbertCore
@testable import AlbertUI

/// Explicitly requested screenshots use synthetic data and an isolated profile.
final class NativeScreenshotTests: DesktopTestCase {
    @MainActor
    func testCaptureApplicationStates() throws {
        guard let destination = ProcessInfo.processInfo.environment["ALBERT_SCREENSHOT_DIR"] else {
            throw XCTSkip("Run python3 apps/macos/desktop_vm.py screenshots to capture native screenshots")
        }
        let assetDirectory = ProcessInfo.processInfo.environment["ALBERT_SCREENSHOT_ASSET_DIR"]
            ?? URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Fixtures").path
        let output = URL(fileURLWithPath: destination, isDirectory: true)
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
        let temporary = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: temporary) }
        let app = NSApplication.shared
        let updates = AppUpdates(enabled: true, currentVersion: "0.1.0", checkAction: {})
        let model = AppModel(store: LocalStore(directory: temporary), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, updates: updates)
        model.suppressNetwork = true
        model.client = try APIClient(baseURL: URL(string: "https://screenshots.invalid")!, token: "synthetic-screenshot-fixture")
        model.connected = true
        let lake = Attachment(id: "lake", name: "Озеро.jpg", mime: "image/jpeg", sizeBytes: 234000)
        let forest = Attachment(id: "forest", name: "Тропа.jpg", mime: "image/jpeg", sizeBytes: 335000)
        model.images[lake.id] = try XCTUnwrap(NSImage(contentsOfFile: assetDirectory + "/lake.jpg"))
        model.images[forest.id] = try XCTUnwrap(NSImage(contentsOfFile: assetDirectory + "/forest.jpg"))
        model.content.files = [lake, forest, Attachment(id: "note", name: "Озеро и лес.md", mime: "text/markdown", sizeBytes: 1280)]
        let tasks = Data(#"[{"id":"photos","title":"Подготовить фотографии для заметки","goal":"Выбрать снимки озера и лесной тропы, подготовить копии для публикации и сохранить оригиналы.","status":"waiting","reason":"Сохранить копии в JPEG или PNG?","version":1},{"id":"availability","title":"Проверять доступность Альберта","goal":"Сообщать о сбоях и восстановлении связи.","status":"running","reason":"Следующая проверка через 5 минут","schedule_seconds":300,"version":1},{"id":"export","title":"Собрать команду экспорта","goal":"Подготовить команду для уменьшения фотографий.","status":"completed","result":"Команда добавлена в разговор.","version":1}]"#.utf8)
        model.content.tasks = try APIJSON.decoder().decode([AlbertTask].self, from: tasks)
        model.content.memory = try APIJSON.decoder().decode([MemoryEntry].self, from: Data(###"[{"path":"Предпочтения.md","content":"Предпочитает короткие ответы и простые интерфейсы."},{"path":"Фотографии.md","content":"## Обработка снимков\n\nОригиналы хранить отдельно. Для заметок готовить копии шириной **1600 пикселей**."}]"###.utf8))
        model.content.messages = [
            Message(id: "user", role: "user", text: "Собери из этих снимков короткую заметку."),
            Message(id: "assistant", role: "assistant", text: "## Озеро и лес\n\nДва кадра для одной заметки: **общий план** и *лесная тропа*.\n\n1. Озеро — первым.\n2. Тропа — следом.\n\n> Оригиналы сохраним отдельно.\n\n```sh\nmagick lake.jpg -resize '1600x>' export/lake.jpg\n```", format: "markdown", delivery: "sent", attachments: [lake, forest]),
        ]
        let controller = ApplicationController(model: model)
        controller.createWindow(show: true)
        defer { controller.window.orderOut(nil); model.shutdown() }
        let window = controller.window!
        window.appearance = NSAppearance(named: .aqua)
        app.appearance = window.appearance
        window.setContentSize(NSSize(width: 820, height: 840))
        func settle() { RunLoop.current.run(until: Date().addingTimeInterval(0.25)) }
        func capture(_ name: String, sheet: Bool = false) throws {
            settle()
            let target = sheet ? try XCTUnwrap(window.attachedSheet) : window
            let view = try XCTUnwrap(target.contentView?.superview ?? target.contentView)
            target.layoutIfNeeded(); view.layoutSubtreeIfNeeded(); view.displayIfNeeded()
            let bitmap = try XCTUnwrap(view.bitmapImageRepForCachingDisplay(in: view.bounds))
            view.cacheDisplay(in: view.bounds, to: bitmap)
            let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
            try data.write(to: output.appendingPathComponent(name + ".png"))
            // Capture a compact 1x view for the gallery; retain the full Retina PNG.
            let preview = try XCTUnwrap(NSBitmapImageRep(bitmapDataPlanes: nil,
                pixelsWide: Int(view.bounds.width), pixelsHigh: Int(view.bounds.height),
                bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0))
            preview.size = view.bounds.size
            view.cacheDisplay(in: view.bounds, to: preview)
            let jpeg = try XCTUnwrap(preview.representation(using: .jpeg, properties: [.compressionFactor: 0.86]))
            try jpeg.write(to: output.appendingPathComponent(name + "-preview.jpg"))
        }
        try capture("conversation-light")
        model.activity = ServerActivity(state: "processing", text: "Альберт думает…")
        try capture("conversation-processing")
        model.activity = nil
        window.appearance = NSAppearance(named: .darkAqua); app.appearance = window.appearance
        try capture("conversation-dark")
        if let sections = window.toolbar?.items.first(where: { $0.itemIdentifier.rawValue == "sections" })?.view as? NSButton {
            window.makeFirstResponder(sections)
            sections.performClick(nil)
            XCTAssertTrue(model.sidebarVisible)
            try capture("sections-focused-dark")
            sections.performClick(nil)
            XCTAssertFalse(model.sidebarVisible)
        } else { XCTFail("Sections toolbar button missing") }
        window.appearance = NSAppearance(named: .aqua); app.appearance = window.appearance
        let conversation = model.content.messages
        model.content.messages = [Message(id: "composer-prompt", role: "assistant", text: "Пришли снимки — выберу порядок и подготовлю заметку.", delivery: "sent")]
        model.draft.text = "Озеро поставь первым. Из этих двух снимков собери короткую заметку."
        model.draft.attachments = [lake, forest]
        window.setContentSize(NSSize(width: 820, height: 580))
        try capture("composer-images-light")
        window.appearance = NSAppearance(named: .darkAqua); app.appearance = window.appearance
        try capture("composer-images-dark")
        window.appearance = NSAppearance(named: .aqua); app.appearance = window.appearance
        model.draft.attachments = []
        model.draft.text = "Обрати внимание на порядок фотографий."
        model.draft.voice = VoiceDraft(durationSeconds: 18)
        try capture("composer-voice-ready")
        model.draft.voice = nil
        model.voiceActivity = .recording; model.recordingSeconds = 14
        try capture("composer-voice-recording")
        model.voiceActivity = .idle; model.recordingSeconds = 0
        model.draft = Draft()
        model.content.messages = conversation
        window.setContentSize(NSSize(width: 820, height: 680))
        model.sidebarVisible = true; model.section = .tasks
        try capture("tasks")
        model.selectedTaskID = "photos"
        try capture("task-detail")
        model.sidebarVisible = false
        model.draft.text = "Сохрани JPEG, оригиналы оставь."
        model.discuss()
        try capture("task-context")
        model.draft.taskID = nil
        model.error = "Нет соединения с сервером."
        model.connected = false
        _ = model.draft.request()
        try capture("offline-draft")
        model.error = nil; model.connected = true
        model.sidebarVisible = true; model.section = .memory
        try capture("memory")
        model.section = .files
        try capture("files")
        model.preview = lake
        try capture("image-viewer", sheet: true)
        model.preview = nil; settle()
        model.serverURL = "https://albert.example.invalid"
        model.showSettings = true
        try capture("connection", sheet: true)
        model.showSettings = false; settle()
        updates.receiveReady(version: "0.1.1") { _ in }
        model.showUpdates = true
        try capture("update-ready", sheet: true)
        model.showUpdates = false; settle()
        XCTAssertEqual(model.draft.text, "Сохрани JPEG, оригиналы оставь.")
    }
}
