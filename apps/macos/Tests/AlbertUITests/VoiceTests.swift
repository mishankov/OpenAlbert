import XCTest
import AppKit
import AVFoundation
import Combine
import AlbertCore
@testable import AlbertUI

@MainActor
final class FakeVoiceRecorder: VoiceRecording {
    var permission = true
    var delayPermission = false
    var permissionContinuation: CheckedContinuation<Bool, Never>?
    var permissionRequests = 0
    var starts = 0
    var stops = 0
    var elapsed: TimeInterval = 0
    var isRecording = false
    var failure: Error?
    var startError: Error?
    func requestPermission() async -> Bool {
        permissionRequests += 1
        if delayPermission { return await withCheckedContinuation { permissionContinuation = $0 } }
        return permission
    }
    func resolvePermission(_ granted: Bool) { permissionContinuation?.resume(returning: granted); permissionContinuation = nil }
    func start(at url: URL) throws {
        if let startError { throw startError }
        starts += 1; isRecording = true
        try Data("fake encoded audio".utf8).write(to: url)
    }
    func stop() -> TimeInterval { stops += 1; isRecording = false; return elapsed }
}

@MainActor
final class FakeAudioPlayer: AudioPlaying {
    var isPlaying = false
    var urls: [URL] = []
    var data: [Data] = []
    var playbackError: Error?
    func play(url: URL) throws { if let playbackError { throw playbackError }; urls.append(url); isPlaying = true }
    func play(data: Data) throws { if let playbackError { throw playbackError }; self.data.append(data); isPlaying = true }
    func stop() { isPlaying = false }
}

private final class VoiceURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (Int, Data))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        do {
            let (status, data) = try Self.handler!(request)
            client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: nil)!, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data); client?.urlProtocolDidFinishLoading(self)
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}

final class VoiceTests: DesktopTestCase {
    @MainActor
    private func model(recorder: FakeVoiceRecorder? = nil, player: FakeAudioPlayer? = nil) -> AppModel {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("albert-voice-test-\(UUID().uuidString)")
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, voiceRecorder: recorder ?? FakeVoiceRecorder(), audioPlayer: player ?? FakeAudioPlayer())
        model.suppressNetwork = true
        addTeardownBlock { try? FileManager.default.removeItem(at: directory); VoiceURLProtocol.handler = nil }
        return model
    }
    @MainActor
    private func waitUntil(_ condition: () -> Bool, file: StaticString = #filePath, line: UInt = #line) async throws {
        for _ in 0..<200 { if condition() { return }; try await Task.sleep(for: .milliseconds(5)) }
        XCTFail("State transition timed out", file: file, line: line)
    }
    private func client() throws -> APIClient {
        let config = URLSessionConfiguration.ephemeral; config.protocolClasses = [VoiceURLProtocol.self]
        return try APIClient(baseURL: URL(string: "https://albert.example")!, token: "private-token", session: URLSession(configuration: config))
    }
    @MainActor
    private func readyVoice(_ model: AppModel, recorder: FakeVoiceRecorder, seconds: Double = 12) async throws {
        model.toggleRecording()
        try await waitUntil { model.voicePhase == .recording }
        recorder.elapsed = seconds
        model.toggleRecording()
        XCTAssertEqual(model.voicePhase, .ready)
    }

    @MainActor
    func testPermissionDeniedAndDeviceErrorKeepTextWithoutStarting() async throws {
        let recorder = FakeVoiceRecorder(); recorder.permission = false
        let model = model(recorder: recorder)
        model.draft.text = "Черновик"
        model.toggleRecording()
        try await waitUntil { model.voicePhase == .idle }
        XCTAssertEqual(recorder.permissionRequests, 1); XCTAssertEqual(recorder.starts, 0)
        XCTAssertEqual(model.draft.text, "Черновик"); XCTAssertNotNil(model.error)
        recorder.permission = true; recorder.startError = VoiceError.recordingFailed
        model.toggleRecording(); try await waitUntil { model.voicePhase == .idle }
        XCTAssertNil(model.draft.voice); XCTAssertEqual(model.draft.text, "Черновик")
    }

    @MainActor
    func testCancelledPermissionAndPermissionBehindDialogNeverStartLate() async throws {
        let recorder = FakeVoiceRecorder(); recorder.delayPermission = true
        let model = model(recorder: recorder); model.draft.text = "Не терять"
        model.toggleRecording(); try await waitUntil { recorder.permissionContinuation != nil }
        model.escape(); recorder.resolvePermission(true)
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(recorder.starts, 0); XCTAssertEqual(model.voicePhase, .idle)
        model.toggleRecording(); try await waitUntil { recorder.permissionContinuation != nil }
        model.showSettings = true
        recorder.resolvePermission(true)
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(recorder.starts, 0); XCTAssertEqual(model.voicePhase, .idle)
        XCTAssertEqual(model.draft.text, "Не терять")
    }

    @MainActor
    func testQuitPreservesReadyVoicePrivatelyAndRelaunchCanPlay() async throws {
        let recorder = FakeVoiceRecorder(), player = FakeAudioPlayer()
        let model = model(recorder: recorder, player: player); model.draft.text = "С записью"
        model.toggleRecording(); try await waitUntil { model.voicePhase == .recording }
        XCTAssertFalse(model.canInstallUpdate); XCTAssertFalse(model.canSend)
        recorder.elapsed = 27
        model.shutdown()
        let voice = try XCTUnwrap(model.draft.voice), url = try XCTUnwrap(voice.fileURL(in: model.store))
        XCTAssertEqual(voice.durationSeconds, 27)
        XCTAssertEqual(try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int, 0o600)
        XCTAssertEqual(try FileManager.default.attributesOfItem(atPath: url.deletingLastPathComponent().path)[.posixPermissions] as? Int, 0o700)
        let loaded = AppModel(store: model.store, defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, voiceRecorder: FakeVoiceRecorder(), audioPlayer: player)
        XCTAssertEqual(loaded.voicePhase, .ready); XCTAssertEqual(loaded.draft.text, "С записью")
        loaded.toggleDraftPlayback(); XCTAssertEqual(player.urls.last, url)
        XCTAssertFalse(loaded.canInstallUpdate)
        loaded.toggleDraftPlayback(); XCTAssertFalse(player.isPlaying)
        XCTAssertTrue(loaded.canInstallUpdate)
        loaded.escape(); XCTAssertNil(loaded.draft.voice); XCTAssertEqual(loaded.draft.text, "С записью")
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.path))
    }

    @MainActor
    func testNavigationAndWindowCloseStopRecordingAndKeepHiddenVoiceOnEscape() async throws {
        let recorder = FakeVoiceRecorder(); let model = model(recorder: recorder)
        model.draft.text = "Текст"
        try await readyVoice(model, recorder: recorder)
        model.navigate(.tasks); model.selectedTaskID = "task"
        model.escape()
        XCTAssertNil(model.selectedTaskID); XCTAssertNotNil(model.draft.voice)
        XCTAssertEqual(model.draft.text, "Текст")
        model.navigate(.conversation); model.cancelVoice()
        model.toggleRecording(); try await waitUntil { model.voicePhase == .recording }
        recorder.elapsed = 8
        model.navigate(.tasks)
        XCTAssertEqual(model.voicePhase, .ready); XCTAssertFalse(recorder.isRecording)
        model.navigate(.conversation); model.cancelVoice()
        let controller = ApplicationController(model: model); controller.createWindow(show: true)
        model.toggleRecording(); try await waitUntil { model.voicePhase == .recording }
        recorder.elapsed = 4
        controller.window.close()
        XCTAssertFalse(recorder.isRecording); XCTAssertEqual(model.draft.voice?.durationSeconds, 4)
        model.cancelVoice(); recorder.delayPermission = true
        controller.window.makeKeyAndOrderFront(nil)
        model.toggleRecording(); try await waitUntil { recorder.permissionContinuation != nil }
        let starts = recorder.starts
        controller.window.close(); recorder.resolvePermission(true)
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(recorder.starts, starts)
    }

    @MainActor
    func testDurationLimitAndPlaybackFailureKeepDraft() async throws {
        let recorder = FakeVoiceRecorder(), player = FakeAudioPlayer()
        let model = model(recorder: recorder, player: player); model.draft.text = "Пять минут"
        model.toggleRecording(); try await waitUntil { model.voicePhase == .recording }
        recorder.elapsed = 301; model.updateRecordingProgress()
        XCTAssertEqual(model.voicePhase, .ready); XCTAssertEqual(model.draft.voice?.durationSeconds, 300)
        XCTAssertFalse(recorder.isRecording)
        player.playbackError = VoiceError.invalidRecording
        model.toggleDraftPlayback()
        XCTAssertNotNil(model.error); XCTAssertNotNil(model.draft.voice); XCTAssertEqual(model.draft.text, "Пять минут")
    }

    @MainActor
    func testUploadOnceRetrySameUUIDAndSuccessfulSendPreservesNewText() async throws {
        let recorder = FakeVoiceRecorder(); let model = model(recorder: recorder); model.client = try client()
        model.draft.text = "Расшифруй"
        try await readyVoice(model, recorder: recorder)
        let file = try XCTUnwrap(model.draft.voice?.fileURL(in: model.store))
        var uploads = 0, requests: [SendRequest] = []
        VoiceURLProtocol.handler = { request in
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer private-token")
            if request.url!.lastPathComponent == "attachments" {
                uploads += 1
                let body = String(decoding: request.voiceTestBody, as: UTF8.self)
                XCTAssertTrue(body.contains("Content-Type: audio/mp4")); XCTAssertTrue(body.contains(".m4a"))
                return (201, Data(#"{"attachment":{"id":"voice-upload","name":"voice.m4a","mime":"audio/mp4","size_bytes":18}}"#.utf8))
            }
            let body = try APIJSON.decoder().decode(SendRequest.self, from: request.voiceTestBody)
            requests.append(body)
            if requests.count == 1 { return (503, Data(#"{"error":{"code":"unavailable","message":"Попробуйте снова"}}"#.utf8)) }
            struct Response: Encodable { let message: Message }
            return (202, try APIJSON.encoder().encode(Response(message: Message(id: "desktop:\(body.clientMessageID)", role: "user", text: body.text))))
        }
        model.submit(); try await waitUntil { !model.sending }
        XCTAssertEqual(uploads, 1); XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(model.draft.voice?.uploadedAttachment?.id, "voice-upload")
        let pending = try XCTUnwrap(model.draft.pending)
        XCTAssertEqual(pending.attachmentIDs, ["voice-upload"])
        XCTAssertTrue(FileManager.default.fileExists(atPath: file.path))
        XCTAssertEqual(model.store.load(Draft.self, name: "draft.json")?.pending, pending)
        model.submit(); model.draft.text = "Новый текст во время отправки"
        try await waitUntil { !model.sending }
        XCTAssertEqual(uploads, 1); XCTAssertEqual(requests.count, 2)
        XCTAssertEqual(requests.first?.clientMessageID, requests.last?.clientMessageID)
        XCTAssertNil(model.draft.voice); XCTAssertNil(model.draft.pending)
        XCTAssertEqual(model.draft.text, "Новый текст во время отправки")
        XCTAssertFalse(FileManager.default.fileExists(atPath: file.path))
    }

    @MainActor
    func testUUIDAndUploadedIDMustPersistBeforeMessagesPOST() async throws {
        let recorder = FakeVoiceRecorder(); let model = model(recorder: recorder); model.client = try client()
        try await readyVoice(model, recorder: recorder)
        let draftURL = model.store.directory.appendingPathComponent("draft.json")
        var uploads = 0, messages = 0
        VoiceURLProtocol.handler = { request in
            if request.url!.lastPathComponent == "attachments" {
                uploads += 1
                return (201, Data(#"{"attachment":{"id":"saved-voice","name":"voice.m4a","mime":"audio/mp4","size_bytes":18}}"#.utf8))
            }
            messages += 1
            return (503, Data(#"{"error":{"code":"unavailable","message":"Retry"}}"#.utf8))
        }
        var failOnUUID = true
        let subscription = model.$draft.sink { draft in
            if (failOnUUID && draft.pending != nil) || (!failOnUUID && draft.voice?.uploadedAttachment != nil) {
                try? FileManager.default.removeItem(at: draftURL)
                try? FileManager.default.createDirectory(at: draftURL, withIntermediateDirectories: true)
            }
        }
        model.submit(); XCTAssertFalse(model.sending)
        XCTAssertEqual(uploads, 0); XCTAssertEqual(messages, 0); XCTAssertNotNil(model.draft.voice)
        XCTAssertNotNil(model.error)
        try FileManager.default.removeItem(at: draftURL)
        failOnUUID = false
        model.submit(); try await waitUntil { !model.sending }
        XCTAssertEqual(uploads, 1); XCTAssertEqual(messages, 0)
        XCTAssertNotNil(model.draft.voice?.uploadedAttachment)
        subscription.cancel()
        try FileManager.default.removeItem(at: draftURL)
        model.submit(); try await waitUntil { !model.sending }
        XCTAssertEqual(uploads, 1); XCTAssertEqual(messages, 1)
    }

    @MainActor
    func testAuthenticatedHistoryAudioAndCancelledDownloadCannotPlayLate() async throws {
        let player = FakeAudioPlayer(); let model = model(player: player); model.client = try client()
        let attachment = Attachment(id: "sent-voice", name: "voice.m4a", mime: "audio/mp4", sizeBytes: 3)
        VoiceURLProtocol.handler = { request in
            XCTAssertEqual(request.url?.path, "/api/v1/attachments/sent-voice/content")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer private-token")
            return (200, Data("aac".utf8))
        }
        model.playAttachment(attachment); model.stopPlayback()
        try await Task.sleep(for: .milliseconds(20)); XCTAssertTrue(player.data.isEmpty)
        model.playAttachment(attachment); try await waitUntil { model.playingAudioID == attachment.id }
        XCTAssertEqual(player.data, [Data("aac".utf8)])
        model.playAttachment(attachment); XCTAssertNil(model.playingAudioID); XCTAssertFalse(player.isPlaying)
    }

    @MainActor
    func testNativeVoiceKeyboardLayoutsRepeatIMEAndAttachmentDeletion() async throws {
        let recorder = FakeVoiceRecorder(), player = FakeAudioPlayer(); let model = model(recorder: recorder, player: player)
        let controller = ApplicationController(model: model); controller.createWindow(show: true)
        defer { controller.window.close() }
        try await waitUntil { model.composer != nil }
        let editor = try XCTUnwrap(model.composer); controller.window.makeFirstResponder(editor)
        model.draft.text = "Сохранить текст"
        let record = KeyboardSmoke.event(code: 15, characters: "К", modifiers: [.command, .shift])
        XCTAssertTrue(controller.window.performKeyEquivalent(with: record))
        try await waitUntil { model.voicePhase == .recording }
        XCTAssertTrue(controller.window.performKeyEquivalent(with: KeyboardSmoke.event(code: 15, characters: "К", modifiers: [.command, .shift], repeatKey: true)))
        XCTAssertEqual(model.voicePhase, .recording)
        recorder.elapsed = 5
        _ = controller.window.performKeyEquivalent(with: record)
        XCTAssertEqual(model.voicePhase, .ready)
        let play = KeyboardSmoke.event(code: 35, characters: "З", modifiers: [.command, .shift])
        _ = controller.window.performKeyEquivalent(with: play)
        XCTAssertEqual(player.urls.count, 1)
        _ = controller.window.performKeyEquivalent(with: KeyboardSmoke.event(code: 35, characters: "З", modifiers: [.command, .shift], repeatKey: true))
        XCTAssertTrue(player.isPlaying)
        editor.setMarkedText("に", selectedRange: NSRange(location: 1, length: 0), replacementRange: NSRange(location: NSNotFound, length: 0))
        _ = controller.window.performKeyEquivalent(with: play)
        XCTAssertTrue(player.isPlaying); editor.unmarkText()
        let committedText = model.draft.text
        model.cancelVoice(); XCTAssertEqual(model.draft.text, committedText)
        let attachment = Attachment(id: "image", name: "photo.jpg", mime: "image/jpeg", sizeBytes: 10)
        model.draft.attachments = [attachment]; model.selectedDraftAttachmentID = attachment.id
        model.sending = true
        XCTAssertFalse(controller.handleCommandKeyEquivalent(KeyboardSmoke.event(code: 51, characters: "\u{8}")))
        XCTAssertEqual(model.draft.attachments.count, 1)
        model.sending = false
        XCTAssertTrue(controller.handleCommandKeyEquivalent(KeyboardSmoke.event(code: 51, characters: "\u{8}")))
        XCTAssertTrue(model.draft.attachments.isEmpty); XCTAssertEqual(model.draft.text, committedText)
        XCTAssertEqual(controller.commands["recordVoice"]?.keyEquivalentModifierMask, [.command, .shift])
        XCTAssertEqual(controller.commands["playDraftVoice"]?.keyEquivalent, "p")
    }

    @MainActor
    func testWrappedComposerMeasuresVisualLinesAndCapsHeight() async throws {
        let model = model(); let controller = ApplicationController(model: model)
        controller.createWindow(show: true); controller.window.setContentSize(NSSize(width: 640, height: 500))
        defer { controller.window.close() }
        model.draft.text = "Озеро поставь первым. Из этих двух снимков собери короткую заметку."
        try await waitUntil { model.composerHeight > 32 }
        XCTAssertLessThanOrEqual(model.composerHeight, 120)
        model.draft.text = String(repeating: "Длинное сообщение без явного переноса строки. ", count: 100)
        try await waitUntil { model.composerHeight == 120 }
    }

    @MainActor
    func testProductionAACSettingsEncodeWithoutMicrophone() throws {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("voice-\(UUID().uuidString).m4a")
        defer { try? FileManager.default.removeItem(at: url) }
        let format = try XCTUnwrap(AVAudioFormat(standardFormatWithSampleRate: 16_000, channels: 1))
        let buffer = try XCTUnwrap(AVAudioPCMBuffer(pcmFormat: format, frameCapacity: 16_000))
        buffer.frameLength = 16_000
        memset(buffer.floatChannelData![0], 0, Int(buffer.frameLength) * MemoryLayout<Float>.size)
        var file: AVAudioFile? = try AVAudioFile(forWriting: url, settings: MacVoiceRecorder.settings)
        try file?.write(from: buffer); file = nil
        let decoded = try AVAudioFile(forReading: url)
        XCTAssertGreaterThan(decoded.length, 0)
        XCTAssertEqual(decoded.processingFormat.channelCount, 1)
        XCTAssertEqual(decoded.processingFormat.sampleRate, 16_000)
        XCTAssertTrue(String(decoding: try Data(contentsOf: url).prefix(32), as: UTF8.self).contains("ftyp"))
    }
}

private extension URLRequest {
    var voiceTestBody: Data {
        if let httpBody { return httpBody }
        guard let stream = httpBodyStream else { return Data() }
        stream.open(); defer { stream.close() }
        var data = Data(), buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let count = stream.read(&buffer, maxLength: buffer.count)
            if count <= 0 { break }; data.append(buffer, count: count)
        }
        return data
    }
}

extension VoiceTests {
    @MainActor
    func testPermissionInstructionSurvivesSuccessfulPolling() async throws {
        let recorder = FakeVoiceRecorder(); recorder.permission = false
        let model = model(recorder: recorder); model.client = try client()
        model.toggleRecording(); try await waitUntil { model.voicePhase == .idle }
        let instruction = try XCTUnwrap(model.error)
        VoiceURLProtocol.handler = { request in
            switch request.url!.lastPathComponent {
            case "messages": return (200, Data(#"{"messages":[],"has_more":false,"next_before":null}"#.utf8))
            case "status": return (200, Data(#"{"api_version":1,"name":"Альберт","conversation_id":"personal:1","max_attachment_bytes":20971520}"#.utf8))
            case "tasks": return (200, Data(#"{"tasks":[]}"#.utf8))
            case "memory": return (200, Data(#"{"entries":[]}"#.utf8))
            default: return (200, Data(#"{"files":[]}"#.utf8))
            }
        }
        model.suppressNetwork = false
        await model.refresh(force: true)
        XCTAssertEqual(model.error, instruction); XCTAssertTrue(model.errorIsLocalVoice)
        model.cancelVoice(); XCTAssertNil(model.error)
    }

    @MainActor
    func testDraftAttachmentCommandsCannotRemoveOrOpenHiddenAttachments() throws {
        let model = model()
        let attachment = Attachment(id: "image", name: "image.jpg", mime: "image/jpeg", sizeBytes: 10)
        model.draft.attachments = [attachment]; model.selectedDraftAttachmentID = attachment.id
        model.navigate(.tasks)
        XCTAssertNil(model.selectedDraftAttachmentID)
        model.selectedDraftAttachmentID = attachment.id // Even stale selection is harmless.
        XCTAssertFalse(model.canRemoveDraftAttachment)
        model.removeDraftAttachment(); model.removeDraftAttachment(attachment.id); model.openDraftAttachment()
        XCTAssertEqual(model.draft.attachments, [attachment]); XCTAssertNil(model.preview)
    }

    @MainActor
    func testPersistenceFailureDoesNotDeleteStoppedRecording() async throws {
        let recorder = FakeVoiceRecorder(); let model = model(recorder: recorder)
        model.draft.text = "Останется"
        model.toggleRecording(); try await waitUntil { model.voicePhase == .recording }
        let url = try XCTUnwrap(model.draft.voice?.fileURL(in: model.store))
        let draftURL = model.store.directory.appendingPathComponent("draft.json")
        try FileManager.default.removeItem(at: draftURL)
        try FileManager.default.createDirectory(at: draftURL, withIntermediateDirectories: true)
        recorder.elapsed = 4; model.finishRecording()
        XCTAssertTrue(FileManager.default.fileExists(atPath: url.path))
        XCTAssertEqual(model.voicePhase, .ready); XCTAssertEqual(model.draft.voice?.durationSeconds, 4)
        XCTAssertNotNil(model.error); XCTAssertEqual(model.draft.text, "Останется")
    }
}
