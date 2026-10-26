import AppKit
import SwiftUI
import AlbertCore

@MainActor
private final class CommandWindow: NSWindow {
    var commandHandler: ((NSEvent) -> Bool)?
    override func performKeyEquivalent(with event: NSEvent) -> Bool {
        if commandHandler?(event) == true { return true }
        return super.performKeyEquivalent(with: event)
    }
}

@MainActor
public final class ApplicationController: NSObject, NSApplicationDelegate, NSMenuItemValidation, NSToolbarDelegate, NSWindowDelegate {
    public let model: AppModel
    public private(set) var window: NSWindow!
    public private(set) var commands: [String: NSMenuItem] = [:]
    private var observers: [NSObjectProtocol] = []
    public init(model: AppModel) { self.model = model; super.init() }
    public func applicationDidFinishLaunching(_ notification: Notification) { createWindow(); model.updates.start() }
    public func createWindow(show: Bool = true) {
        guard window == nil else { return }
        window = CommandWindow(contentRect: NSRect(x: 0, y: 0, width: 900, height: 720), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        (window as? CommandWindow)?.commandHandler = { [weak self] in self?.handleCommandKeyEquivalent($0) == true }
        window.title = "Альберт"
        window.minSize = NSSize(width: 640, height: 500)
        window.setFrameAutosaveName("AlbertMainWindow")
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.contentView = NSHostingView(rootView: RootView(model: model))
        let toolbar = NSToolbar(identifier: "AlbertToolbar")
        // The custom button already draws its title; a toolbar label would duplicate it.
        toolbar.delegate = self; toolbar.displayMode = .iconOnly
        if #available(macOS 15.0, *) { toolbar.allowsDisplayModeCustomization = false }
        window.toolbar = toolbar; window.toolbarStyle = .unified
        buildMenu()
        if show { window.center(); window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true) }
        observers.append(NotificationCenter.default.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in await self?.model.refresh(force: true) }
        })
    }
    public func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool { window.makeKeyAndOrderFront(nil); return true }
    public func windowWillClose(_ notification: Notification) { model.prepareVoiceForQuit() }
    public func applicationWillTerminate(_ notification: Notification) { model.shutdown() }
    public func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        model.prepareVoiceForQuit()
        model.updates.cancelForQuit()
        return .terminateNow
    }
    public func toolbarAllowedItemIdentifiers(_ toolbar: NSToolbar) -> [NSToolbarItem.Identifier] { [.init("sections"), .flexibleSpace] }
    public func toolbarDefaultItemIdentifiers(_ toolbar: NSToolbar) -> [NSToolbarItem.Identifier] { [.init("sections"), .flexibleSpace] }
    public func toolbar(_ toolbar: NSToolbar, itemForItemIdentifier id: NSToolbarItem.Identifier, willBeInsertedIntoToolbar flag: Bool) -> NSToolbarItem? {
        guard id.rawValue == "sections" else { return nil }
        let item = NSToolbarItem(itemIdentifier: id)
        let button = NSButton(title: "Разделы", target: self, action: #selector(toggleSidebar))
        button.bezelStyle = .texturedRounded; button.isBordered = false; button.font = .systemFont(ofSize: 12)
        button.toolTip = "Разделы · ⌘\\"; button.setAccessibilityIdentifier("toggleSections")
        button.keyEquivalent = "\\"; button.keyEquivalentModifierMask = .command
        item.view = button; item.label = "Разделы"; item.paletteLabel = "Разделы"
        return item
    }
    private func buildMenu() {
        let main = NSMenu()
        func submenu(_ title: String) -> NSMenu { let item = NSMenuItem(); let sub = NSMenu(title: title); item.submenu = sub; main.addItem(item); return sub }
        func command(_ menu: NSMenu, _ title: String, _ action: Selector, _ key: String = "", _ modifiers: NSEvent.ModifierFlags = .command, id: String? = nil) {
            let item = NSMenuItem(title: title, action: action, keyEquivalent: key)
            item.target = self; item.keyEquivalentModifierMask = modifiers
            if let id { item.identifier = .init(id); commands[id] = item }
            menu.addItem(item)
        }
        let app = submenu("Альберт")
        command(app, "Об Альберте", #selector(about))
        command(app, "Обновления…", #selector(openUpdates), "u", [.command, .shift], id: "updates")
        app.addItem(.separator())
        command(app, "Подключение…", #selector(settings), ",", id: "settings")
        app.addItem(.separator())
        let hide = NSMenuItem(title: "Скрыть Альберта", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h"); app.addItem(hide)
        app.addItem(.separator())
        let quit = NSMenuItem(title: "Завершить Альберт", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q"); app.addItem(quit)
        let edit = submenu("Правка")
        for (title, selector, key) in [("Отменить", Selector(("undo:")), "z"), ("Вырезать", #selector(NSText.cut(_:)), "x"), ("Скопировать", #selector(NSText.copy(_:)), "c"), ("Вставить", #selector(NSText.paste(_:)), "v"), ("Выбрать всё", #selector(NSText.selectAll(_:)), "a")] {
            edit.addItem(NSMenuItem(title: title, action: selector, keyEquivalent: key))
        }
        let redo = NSMenuItem(title: "Повторить ввод", action: Selector(("redo:")), keyEquivalent: "z"); redo.keyEquivalentModifierMask = [.command, .shift]; edit.addItem(redo)
        let actions = submenu("Действия")
        command(actions, "Отправить", #selector(primary), "\r", id: "primary")
        command(actions, "Записать / закончить голосовое", #selector(recordVoice), "r", [.command, .shift], id: "recordVoice")
        command(actions, "Прослушать / остановить запись", #selector(playDraftVoice), "p", [.command, .shift], id: "playDraftVoice")
        command(actions, "Остановить", #selector(stop), ".", id: "stop")
        command(actions, "Прикрепить файл…", #selector(attach), "o", id: "attach")
        command(actions, "Открыть вложение черновика", #selector(openDraftAttachment), "o", [.command, .shift], id: "openDraftAttachment")
        command(actions, "Убрать вложение черновика", #selector(removeDraftAttachment), "\u{8}", .command, id: "removeDraftAttachment")
        command(actions, "Сохранить файл…", #selector(save), "s", id: "save")
        command(actions, "Повторить", #selector(retry), "r", id: "retry")
        let view = submenu("Вид")
        command(view, "Разделы", #selector(toggleSidebar), "\\", id: "sidebar")
        for section in Section.allCases {
            command(view, section.title, #selector(navigate(_:)), "\(section.rawValue)", id: "section\(section.rawValue)")
            view.items.last?.tag = section.rawValue
        }
        command(view, "Назад", #selector(cancel), "\u{1b}", [], id: "escape")
        command(view, "Предыдущие сообщения", #selector(older), String(UnicodeScalar(NSUpArrowFunctionKey)!), [.command, .control], id: "older")
        NSApp.mainMenu = main
    }
    public func validateMenuItem(_ menuItem: NSMenuItem) -> Bool {
        let modal = model.modalPresented
        switch menuItem.identifier?.rawValue {
        case "primary":
            menuItem.title = model.section == .tasks && model.selectedTask != nil ? "Обсудить поручение" : "Отправить"
            return !modal && (model.section == .tasks ? model.selectedTask != nil && !model.sending : model.section == .conversation && model.canSend)
        case "recordVoice": return model.canToggleRecording && model.composer?.hasMarkedText() != true
        case "playDraftVoice": return model.canPlayDraft && model.composer?.hasMarkedText() != true
        case "stop": return !modal && model.canStop
        case "attach": return !modal && model.client != nil && !model.sending && !model.uploading && !model.voiceBusy && model.attachmentCount < 10
        case "openDraftAttachment": return model.section == .conversation && !modal && !model.sending && !model.uploading && model.draft.attachments.contains { $0.id == model.selectedDraftAttachmentID }
        case "removeDraftAttachment": return model.canRemoveDraftAttachment
        case "save": return !model.showSettings && model.selectedFile != nil && !model.downloading && NSApp.keyWindow?.attachedSheet == nil
        case "retry": return !modal && !model.errorIsLocalVoice && model.client != nil && !model.sending
        case "older": return !modal && model.section == .conversation && model.nextBefore != nil && !model.loadingOlder
        case "settings": return model.canOpenSettings
        case "updates":
            menuItem.title = model.updates.phase == .ready ? "Обновление готово…" : "Обновления…"
            return !modal && !model.voiceBusy
        case "escape": return KeyboardGate.permitsPrimaryAction() && NSApp.modalWindow == nil && !model.connecting && model.updates.phase != .installing
        default: return !modal
        }
    }
    /// Physical key codes keep the commands available with Russian and other layouts.
    /// Unselected Cmd-Delete remains the native editor's delete-to-line-start command.
    public func handleCommandKeyEquivalent(_ event: NSEvent) -> Bool {
        guard event.type == .keyDown else { return false }
        let flags = event.modifierFlags.intersection([.command, .shift, .option, .control])
        if flags == [.command, .shift], [15, 35].contains(event.keyCode) {
            guard KeyboardGate.permitsPrimaryAction(event: event, responder: window?.firstResponder) else { return true }
            if event.keyCode == 15 { model.toggleRecording() } else { model.toggleDraftPlayback() }
            return true
        }
        if flags == .command, event.keyCode == 51, model.canRemoveDraftAttachment {
            guard KeyboardGate.permitsPrimaryAction(event: event, responder: window?.firstResponder) else { return true }
            model.removeDraftAttachment(); return true
        }
        return false
    }
    @objc public func recordVoice() { model.toggleRecording() }
    @objc public func playDraftVoice() { model.toggleDraftPlayback() }
    @objc public func primary() {
        guard !model.modalPresented, KeyboardGate.permitsPrimaryAction() else { return }
        if model.section == .tasks { model.discuss() }
        else if model.section == .conversation { model.submit() }
    }
    @objc public func toggleSidebar() { guard !model.modalPresented, KeyboardGate.permitsPrimaryAction() else { return }; model.sidebarVisible.toggle() }
    @objc public func navigate(_ item: NSMenuItem) { guard let section = Section(rawValue: item.tag), KeyboardGate.permitsPrimaryAction() else { return }; model.navigate(section) }
    @objc public func settings() { model.openSettings() }
    @objc public func openUpdates() { model.openUpdates() }
    @objc public func stop() { model.stop() }
    @objc public func attach() { guard KeyboardGate.permitsPrimaryAction() else { return }; model.attach() }
    @objc public func openDraftAttachment() { model.openDraftAttachment() }
    @objc public func removeDraftAttachment() { model.removeDraftAttachment() }
    @objc public func save() { guard let selected = model.preview ?? model.selectedFile, KeyboardGate.permitsPrimaryAction() else { return }; model.save(selected) }
    @objc public func retry() { model.retry() }
    @objc public func cancel() { guard KeyboardGate.permitsPrimaryAction(), NSApp.modalWindow == nil else { return }; model.escape() }
    @objc public func older() { guard !model.modalPresented, KeyboardGate.permitsPrimaryAction() else { return }; model.loadOlder() }
    @objc private func about() { NSApp.orderFrontStandardAboutPanel(options: [.applicationName: "Альберт", .applicationVersion: model.updates.currentVersion]) }
}

@MainActor
public enum AlbertApplication {
    public static func run() {
        let arguments = CommandLine.arguments
        let snapshot = arguments.contains("--snapshot") || arguments.contains { $0.hasPrefix("--snapshot=") }
        let smoke = arguments.contains("--smoke-test") || snapshot
        // Gate synthetic desktop execution before AppKit or a real user profile is initialized.
        if smoke {
            do { try DesktopIsolation.requireAllowed() }
            catch {
                FileHandle.standardError.write(Data((error.localizedDescription + "\n").utf8))
                exit(78)
            }
        }
        var snapshotDirectory = ProcessInfo.processInfo.environment["ALBERT_SNAPSHOT_DIR"]
        if let value = arguments.first(where: { $0.hasPrefix("--snapshot=") }) { snapshotDirectory = String(value.dropFirst("--snapshot=".count)) }
        else if let index = arguments.firstIndex(of: "--snapshot"), arguments.indices.contains(index + 1), !arguments[index + 1].hasPrefix("-") { snapshotDirectory = arguments[index + 1] }
        if snapshot && (snapshotDirectory?.isEmpty != false) {
            FileHandle.standardError.write(Data("Use --snapshot <output-directory> inside the desktop VM.\n".utf8))
            exit(64)
        }
        let app = NSApplication.shared
        app.setActivationPolicy(.regular)
        let temporary = FileManager.default.temporaryDirectory.appendingPathComponent("albert-smoke-\(UUID().uuidString)")
        let model = smoke ? AppModel(store: LocalStore(directory: temporary), defaults: UserDefaults(suiteName: "albert-smoke-\(UUID().uuidString)")!, startNetwork: false) : AppModel()
        let controller = ApplicationController(model: model)
        app.delegate = controller
        if smoke {
            controller.createWindow(show: true)
            var result = KeyboardSmoke.run(controller: controller)
            if let path = snapshotDirectory {
                do { try KeyboardSmoke.snapshots(controller: controller, directory: URL(fileURLWithPath: path), imagePath: ProcessInfo.processInfo.environment["ALBERT_SMOKE_IMAGE_PATH"]) }
                catch { print("Snapshot failed: \(error.localizedDescription)"); result = false }
            }
            model.shutdown()
            try? FileManager.default.removeItem(at: temporary)
            print(result ? "ALBERT_SMOKE_PASS" : "ALBERT_SMOKE_FAIL")
            controller.window.orderOut(nil)
            exit(result ? 0 : 1)
        }
        app.run()
        withExtendedLifetime(controller) {}
    }
}
