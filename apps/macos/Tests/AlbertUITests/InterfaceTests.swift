import XCTest
import AppKit
import AlbertCore
@testable import AlbertUI

final class InterfaceTests: DesktopTestCase {
    @MainActor
    func testNativeKeyboardFlowAndSharedActions() throws {
        let app = NSApplication.shared
        app.setActivationPolicy(.regular)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        let controller = ApplicationController(model: model)
        controller.createWindow(show: true)
        defer { controller.window.orderOut(nil); model.shutdown() }
        XCTAssertTrue(KeyboardSmoke.run(controller: controller))
        XCTAssertEqual(controller.commands["primary"]?.keyEquivalent, "\r")
        XCTAssertEqual(controller.commands["stop"]?.keyEquivalent, ".")
        XCTAssertEqual(controller.commands["attach"]?.keyEquivalent, "o")
        XCTAssertEqual(controller.commands["save"]?.keyEquivalent, "s")
        XCTAssertEqual(controller.commands["settings"]?.keyEquivalent, ",")
        // Shift+Return inserts a newline through the native text system.
        guard let editor = model.composer else { return XCTFail("native editor missing") }
        var sends = 0; editor.onSend = { sends += 1 }
        editor.string = "До"; editor.setSelectedRange(NSRange(location: 2, length: 0))
        editor.insertText(" после", replacementRange: NSRange(location: NSNotFound, length: 0))
        editor.keyDown(with: KeyboardSmoke.event(code: 36, characters: "\r", modifiers: .shift))
        XCTAssertEqual(editor.string, "До после\n")
        XCTAssertEqual(sends, 0)
        XCTAssertEqual(editor.accessibilityIdentifier(), "messageEditor")
        // A disabled send command and button call the same guarded model action.
        model.draft.text = "Нет подключения"
        controller.primary()
        model.submit()
        XCTAssertEqual(model.draft.text, "Нет подключения")
        XCTAssertFalse(model.sending)
        // Dialogs block background commands and preserve the draft.
        model.showSettings = true
        controller.toggleSidebar()
        controller.primary()
        XCTAssertFalse(model.sidebarVisible)
        XCTAssertEqual(model.draft.text, "Нет подключения")
        model.showSettings = false
    }

    @MainActor
    func testTaskContextPreservesDraftAndSendingGuardsEscape() throws {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        let data = Data(#"{"id":"task-1","title":"Проверка","goal":"Цель","status":"waiting","reason":"Нужен ответ","version":1}"#.utf8)
        model.content.tasks = [try APIJSON.decoder().decode(AlbertTask.self, from: data)]
        model.draft.text = "Уже набранный текст"
        model.section = .tasks; model.selectedTaskID = "task-1"
        model.discuss()
        XCTAssertEqual(model.section, .conversation)
        XCTAssertEqual(model.draft.taskID, "task-1")
        XCTAssertEqual(model.draft.text, "Уже набранный текст")
        model.sending = true; model.escape()
        XCTAssertEqual(model.draft.taskID, "task-1")
        model.sending = false; model.escape()
        XCTAssertNil(model.draft.taskID)
        XCTAssertEqual(model.draft.text, "Уже набранный текст")
    }

    func testMarkdownHasReadableBlocksAndNoWebExecution() {
        let source = "# Заголовок\n\n**Жирный** и *курсив*\n\n- Один\n2. Два\n> Цитата\n\n```sh\necho hello\n```\n\n<script>alert(1)</script>"
        XCTAssertEqual(MarkdownParser.blocks(source), [.heading(1, "Заголовок"), .paragraph("**Жирный** и *курсив*"), .item("•", "Один"), .item("2.", "Два"), .quote("Цитата"), .code("sh", "echo hello"), .paragraph("<script>alert(1)</script>")])
    }
}

private final class InterfaceURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> Data)?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        do {
            let data = try Self.handler!(request)
            client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}

extension InterfaceTests {
    @MainActor
    func testReconnectFillsGapAfterMoreThanOnePageArrived() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory); InterfaceURLProtocol.handler = nil }
        let local = LocalStore(directory: directory)
        var cached = CachedContent()
        func message(_ n: Int) -> Message { Message(id: "m\(n)", role: "user", text: "\(n)", createdAt: Date(timeIntervalSince1970: Double(n))) }
        cached.messages = (1...200).map(message)
        cached.nextBefore = "older-cursor"
        try local.save(cached, name: "cache.json")
        let model = AppModel(store: local, defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        XCTAssertEqual(model.nextBefore, "older-cursor")
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [InterfaceURLProtocol.self]
        model.client = try APIClient(baseURL: URL(string: "https://albert.example")!, token: "test", session: URLSession(configuration: config))
        var pagesFetched = 0
        InterfaceURLProtocol.handler = { request in
            switch request.url!.lastPathComponent {
            case "messages":
                pagesFetched += 1
                let before = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)!.queryItems?.first { $0.name == "before" }?.value
                let range = before == nil ? 251...350 : 151...250
                return try APIJSON.encoder().encode(MessagePage(messages: range.map(message), hasMore: true, nextBefore: before == nil ? "250" : "150"))
            case "status": return Data(#"{"api_version":1,"name":"Альберт","conversation_id":"personal:1","max_attachment_bytes":20971520}"#.utf8)
            case "tasks": return Data(#"{"tasks":[]}"#.utf8)
            case "memory": return Data(#"{"entries":[]}"#.utf8)
            default: return Data(#"{"files":[]}"#.utf8)
            }
        }
        await model.refresh(force: true)
        XCTAssertEqual(pagesFetched, 2)
        XCTAssertEqual(model.content.messages.count, 350)
        XCTAssertEqual(Set(model.content.messages.map(\.id)).count, 350)
        XCTAssertEqual(model.nextBefore, "older-cursor")
        XCTAssertEqual(local.load(CachedContent.self, name: "cache.json")?.nextBefore, "older-cursor")
    }
}

extension InterfaceTests {
    @MainActor
    func testReturnUsesAPIAndKeepsDraftEditedDuringSend() async throws {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory); InterfaceURLProtocol.handler = nil }
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        model.suppressNetwork = true // Isolate sends from periodic reads.
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [InterfaceURLProtocol.self]
        model.client = try APIClient(baseURL: URL(string: "https://albert.example")!, token: "test", session: URLSession(configuration: config))
        let controller = ApplicationController(model: model)
        controller.createWindow(show: true)
        defer { controller.window.orderOut(nil); model.shutdown() }
        controller.window.layoutIfNeeded()
        await Task.yield()
        guard let editor = model.composer else { return XCTFail("native editor missing") }
        controller.window.makeFirstResponder(editor)
        var requests: [SendRequest] = []
        InterfaceURLProtocol.handler = { incoming in
            var data = incoming.httpBody ?? Data()
            if let stream = incoming.httpBodyStream {
                stream.open(); defer { stream.close() }
                var buffer = [UInt8](repeating: 0, count: 4096)
                while stream.hasBytesAvailable {
                    let count = stream.read(&buffer, maxLength: buffer.count)
                    if count <= 0 { break }; data.append(buffer, count: count)
                }
            }
            let request = try APIJSON.decoder().decode(SendRequest.self, from: data)
            requests.append(request)
            let message = Message(id: "desktop:\(request.clientMessageID)", role: "user", text: request.text)
            struct Response: Encodable { let message: Message }
            return try APIJSON.encoder().encode(Response(message: message))
        }
        model.draft.text = "Первое сообщение"
        editor.keyDown(with: KeyboardSmoke.event(code: 36, characters: "\r", modifiers: []))
        XCTAssertTrue(model.sending)
        editor.keyDown(with: KeyboardSmoke.event(code: 36, characters: "\r", modifiers: [], repeatKey: true))
        model.draft.text = "Следующий черновик"
        for _ in 0..<100 where model.sending { try await Task.sleep(for: .milliseconds(10)) }
        XCTAssertFalse(model.sending)
        XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(requests.first?.text, "Первое сообщение")
        XCTAssertEqual(model.draft.text, "Следующий черновик")
        XCTAssertNil(model.draft.pending)
        XCTAssertEqual(model.content.messages.first?.text, "Первое сообщение")
        // The visible button's model action is the same action as the shortcut.
        model.submit()
        for _ in 0..<100 where model.sending { try await Task.sleep(for: .milliseconds(10)) }
        XCTAssertEqual(requests.count, 2)
        XCTAssertNotEqual(requests.first?.clientMessageID, requests.last?.clientMessageID)
        XCTAssertTrue(model.draft.text.isEmpty)
    }
}

extension InterfaceTests {
    @MainActor
    func testReturnCommitsIMEWithoutSendingAndHandlesKeypad() throws {
        let editor = ComposerTextView()
        var sends = 0
        editor.onSend = { sends += 1 }
        editor.setMarkedText("に", selectedRange: NSRange(location: 1, length: 0), replacementRange: NSRange(location: NSNotFound, length: 0))
        editor.keyDown(with: KeyboardSmoke.event(code: 36, characters: "\r", modifiers: []))
        XCTAssertEqual(sends, 0)
        editor.unmarkText()
        editor.keyDown(with: KeyboardSmoke.event(code: 76, characters: "\u{3}", modifiers: []))
        XCTAssertEqual(sends, 1)
        editor.keyDown(with: KeyboardSmoke.event(code: 76, characters: "\u{3}", modifiers: [], repeatKey: true))
        XCTAssertEqual(sends, 1)
    }

    @MainActor
    func testActivityReflectsServerAndClearsOnCompletionAndDisconnect() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory); InterfaceURLProtocol.handler = nil }
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [InterfaceURLProtocol.self]
        model.client = try APIClient(baseURL: URL(string: "http://192.168.1.31:8080")!, token: "test", session: URLSession(configuration: config))
        var state = "queued"
        var fail = false
        InterfaceURLProtocol.handler = { request in
            if fail { throw URLError(.notConnectedToInternet) }
            switch request.url!.lastPathComponent {
            case "status": return Data("{\"api_version\":1,\"name\":\"Альберт\",\"conversation_id\":\"personal:1\",\"max_attachment_bytes\":20971520,\"activity\":{\"state\":\"\(state)\"}}".utf8)
            case "messages": return Data(#"{"messages":[],"has_more":false,"next_before":null}"#.utf8)
            case "tasks": return Data(#"{"tasks":[]}"#.utf8)
            case "memory": return Data(#"{"entries":[]}"#.utf8)
            default: return Data(#"{"files":[]}"#.utf8)
            }
        }
        model.sending = true
        XCTAssertEqual(model.activityLabel, "Отправляю…")
        model.sending = false
        await model.refresh()
        XCTAssertEqual(model.activityLabel, "Сообщение принято…")
        state = "processing"; await model.refresh()
        XCTAssertEqual(model.activityLabel, "Альберт думает…")
        state = "idle"; await model.refresh()
        XCTAssertNil(model.activityLabel)
        XCTAssertFalse(model.awaitingResponse)
        state = "processing"; await model.refresh()
        fail = true; await model.refresh()
        XCTAssertNil(model.activityLabel)
        XCTAssertFalse(model.connected)
        XCTAssertNotNil(model.error)
    }
}
