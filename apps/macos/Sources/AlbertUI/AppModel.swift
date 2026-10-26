import AppKit
import SwiftUI
import UniformTypeIdentifiers
import AlbertCore
import Combine

public enum Section: Int, CaseIterable, Identifiable {
    case conversation = 1, tasks, memory, files
    public var id: Int { rawValue }
    public var title: String { switch self { case .conversation: return "Разговор"; case .tasks: return "Поручения"; case .memory: return "Память"; case .files: return "Файлы" } }
}

@MainActor
public final class AppModel: ObservableObject {
    @Published public var section: Section = .conversation
    @Published public var sidebarVisible = false
    @Published public var selectedTaskID: String?
    @Published public var draft: Draft { didSet { persistDraft() } }
    @Published public var content: CachedContent
    @Published public var error: String?
    @Published public var voiceActivity: VoicePhase = .idle
    @Published public var recordingSeconds: TimeInterval = 0
    @Published public var playingAudioID: String?
    @Published public var loadingAudioID: String?
    public let voiceRecorder: VoiceRecording
    public let audioPlayer: AudioPlaying
    private var permissionID: UUID?
    private var recordingID: String?
    private var voiceErrorMessage: String?
    private var recordingTask: Task<Void, Never>?
    private var playbackTask: Task<Void, Never>?
    private var playbackID: UUID?
    @Published public var activity: ServerActivity?
    private var activityRevision = 0
    @Published public var sending = false
    @Published public var uploading = false
    @Published public var stopping = false
    @Published public var loadingOlder = false
    @Published public var nextBefore: String?
    @Published public var showSettings = false
    @Published public var showUpdates = false
    @Published public var connecting = false
    @Published public var serverURL: String
    @Published public var preview: Attachment?
    @Published public var images: [String: NSImage] = [:]
    @Published public var selectedDraftAttachmentID: String?
    @Published public var selectedFile: Attachment?
    @Published public var downloading = false
    @Published public var composerHeight: CGFloat = 32
    @Published public var focusComposer = 0
    @Published public var connected = false
    public weak var composer: ComposerTextView?
    public var client: APIClient?
    public let store: LocalStore
    public let updates: AppUpdates
    private var updateChanges: AnyCancellable?
    private var polling: Task<Void, Never>?
    private var refreshing = false
    private var refreshCount = 0
    private var stopID: String?
    private var defaults: UserDefaults
    private var imageLoads = Set<String>()
    private var cachedData: [String: Data] = [:]
    public var suppressNetwork = false

    public init(store: LocalStore? = nil, defaults: UserDefaults = .standard, startNetwork: Bool = true, updates: AppUpdates? = nil, voiceRecorder: VoiceRecording? = nil, audioPlayer: AudioPlaying? = nil) {
        let directory = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Albert", isDirectory: true)
        self.store = store ?? LocalStore(directory: directory)
        self.updates = updates ?? AppUpdates()
        self.voiceRecorder = voiceRecorder ?? MacVoiceRecorder()
        self.audioPlayer = audioPlayer ?? MacAudioPlayer()
        self.defaults = defaults
        self.draft = self.store.load(Draft.self, name: "draft.json") ?? Draft()
        self.content = self.store.load(CachedContent.self, name: "cache.json") ?? CachedContent()
        self.serverURL = defaults.string(forKey: "serverURL") ?? ""
        self.nextBefore = self.content.nextBefore
        self.updates.installationAllowed = { [weak self] in self?.canInstallUpdate == true }
        self.updates.beforeInstall = { [weak self] in
            guard let self else { throw CocoaError(.userCancelled) }
            try self.prepareForUpdate()
        }
        self.updates.restoreAfterFailure = { [weak self] in
            if self?.client != nil { self?.startPolling() }
        }
        self.updateChanges = self.updates.objectWillChange.sink { [weak self] in self?.objectWillChange.send() }
        if startNetwork {
            if let url = try? APIClient.validatedURL(serverURL), let token = try? Keychain.token(for: url.absoluteString), !token.isEmpty {
                client = try? APIClient(baseURL: url, token: token)
                startPolling()
            } else { showSettings = true }
        }
    }
    deinit { polling?.cancel(); recordingTask?.cancel(); playbackTask?.cancel() }
    public var selectedTask: AlbertTask? { content.tasks.first { $0.id == selectedTaskID } }
    public var contextTask: AlbertTask? { content.tasks.first { $0.id == draft.taskID } }
    public var modalPresented: Bool { showSettings || showUpdates || updates.phase == .installing || preview != nil || NSApp?.modalWindow != nil || NSApp?.keyWindow?.attachedSheet != nil }
    public var canSend: Bool { updates.phase != .installing && !voiceBusy && client != nil && !sending && !uploading && draft.hasContent && draft.text.utf8.count <= 32_000 && attachmentCount <= 10 }
    public var canStop: Bool { client != nil && !stopping }
    public var awaitingResponse: Bool { activity?.isActive ?? (content.messages.last?.role == "user") }
    public var activityLabel: String? {
        if sending { return "Отправляю…" }
        guard connected, let activity, activity.isActive else { return nil }
        if let text = activity.text, !text.isEmpty { return text }
        return activity.state == "queued" ? "Сообщение принято…" : "Альберт думает…"
    }
    public func persistDraft() { try? store.save(draft, name: "draft.json") }
    private func persistCache() { content.nextBefore = nextBefore; try? store.save(content, name: "cache.json") }
    public func shutdown() { prepareVoiceForQuit(); polling?.cancel(); persistDraft(); persistCache() }
    public var canInstallUpdate: Bool {
        !voiceBusy && playingAudioID == nil && loadingAudioID == nil && !sending && !uploading && !downloading && !connecting && !stopping && !loadingOlder
        && !showSettings && preview == nil && NSApp?.modalWindow == nil
        && composer?.hasMarkedText() != true && KeyboardGate.permitsPrimaryAction()
    }
    public func prepareForUpdate() throws {
        guard canInstallUpdate else { throw CocoaError(.userCancelled) }
        try store.save(draft, name: "draft.json")
        var cache = content; cache.nextBefore = nextBefore
        try store.save(cache, name: "cache.json")
        polling?.cancel()
    }
    public var canOpenSettings: Bool { !modalPresented && !voiceBusy && !sending && !uploading }
    public func openSettings() {
        guard canOpenSettings, KeyboardGate.permitsPrimaryAction() else { return }
        showSettings = true
    }
    public func openUpdates() {
        guard !modalPresented, !voiceBusy, composer?.hasMarkedText() != true, KeyboardGate.permitsPrimaryAction() else { return }
        showUpdates = true
    }
    public func closeUpdates() {
        guard updates.phase != .installing, KeyboardGate.permitsPrimaryAction() else { return }
        showUpdates = false
    }

    public func startPolling() {
        polling?.cancel()
        polling = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                if NSApp?.isActive != false { await self.refresh() }
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }
    public func connect(url: String, token: String) async {
        guard !connecting, !sending, !uploading, !voiceBusy else { return }
        connecting = true; error = nil
        defer { connecting = false }
        do {
            let base = try APIClient.validatedURL(url)
            let secret = token.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !secret.isEmpty else { throw APIError.unauthorized }
            let newClient = try APIClient(baseURL: base, token: secret)
            let status = try await newClient.status()
            guard status.apiVersion == 1 else { throw APIError.invalidResponse }
            try Keychain.save(token: secret, origin: base.absoluteString)
            if let oldBase = try? APIClient.validatedURL(serverURL), oldBase != base {
                content = CachedContent(); images = [:]; cachedData = [:]; nextBefore = nil
                draft.attachments = []; draft.taskID = nil; draft.pending = nil
                draft.voice?.uploadedAttachment = nil
                stopPlayback()
                persistCache()
            }
            serverURL = base.absoluteString
            defaults.set(serverURL, forKey: "serverURL")
            activityRevision += 1; activity = status.activity
            client = newClient; connected = true; showSettings = false
            startPolling(); await refresh()
            focusComposer += 1
        } catch { self.error = error.localizedDescription }
    }
    public func refresh(force: Bool = false) async {
        guard let client, !refreshing, !suppressNetwork else { return }
        refreshing = true
        let revision = activityRevision
        defer { refreshing = false }
        do {
            let status = try await client.status()
            guard self.client === client else { return }
            if activityRevision == revision { activity = status.activity }
            let knownIDs = Set(content.messages.map(\.id))
            let page = try await client.messages()
            guard self.client === client else { return }
            if content.messages.isEmpty { nextBefore = page.nextBefore }
            content.merge(page)
            // More than 100 messages may arrive while offline. Fill the gap to the
            // cached history before resuming normal polling; the older-page cursor
            // still belongs to the oldest page the user has loaded.
            var catchup = page
            var seenCursors = Set<String>()
            while !knownIDs.isEmpty, catchup.hasMore,
                  !catchup.messages.contains(where: { knownIDs.contains($0.id) }),
                  let cursor = catchup.nextBefore, seenCursors.insert(cursor).inserted {
                catchup = try await client.messages(before: cursor)
                guard self.client === client else { return }
                content.merge(catchup)
                persistCache()
            }
            connected = true
            if !sending && !uploading && !stopping && draft.pending == nil && !errorIsLocalVoice { error = nil }
            refreshCount += 1
            if force || refreshCount % 5 == 1 {
                async let tasks = client.tasks()
                async let memory = client.memory()
                async let files = client.files()
                let loaded = try await (tasks, memory, files)
                guard self.client === client else { return }
                content.tasks = loaded.0; content.memory = loaded.1; content.files = loaded.2
            }
            persistCache()
        } catch { guard self.client === client else { return }; connected = false; if !errorIsLocalVoice { self.error = error.localizedDescription } }
    }
    public func loadOlder() {
        guard let client, let cursor = nextBefore, !loadingOlder else { return }
        loadingOlder = true
        Task {
            defer { loadingOlder = false }
            do {
                let page = try await client.messages(before: cursor)
                guard self.client === client else { return }
                content.merge(page); nextBefore = page.nextBefore; persistCache()
            } catch { guard self.client === client else { return }; self.error = error.localizedDescription }
        }
    }
    public func navigate(_ destination: Section) {
        guard !modalPresented else { return }
        if voiceActivity == .requestingPermission { cancelVoice() }
        if voiceActivity == .recording { finishRecording() }
        selectedDraftAttachmentID = nil
        section = destination
        if destination == .conversation { focusComposer += 1 }
        Task { await refresh(force: true) }
    }
    public func discuss() {
        guard let selectedTask, !modalPresented, !sending, !voiceBusy else { return }
        draft.taskID = selectedTask.id
        section = .conversation; focusComposer += 1
    }
    public func submit() {
        guard !modalPresented, KeyboardGate.permitsPrimaryAction(), canSend, let client else { return }
        stopPlayback()
        do { try store.save(draft, name: "draft.json") }
        catch { self.error = error.localizedDescription; return }
        var request = draft.request() // ambiguous POST retries use this UUID.
        do { try store.save(draft, name: "draft.json") }
        catch { self.error = error.localizedDescription; return }
        let voice = draft.voice
        let originalAttachments = draft.attachments.map(\.id)
        activityRevision += 1
        sending = true; error = nil
        Task {
            defer { sending = false }
            do {
                if let voice, voice.uploadedAttachment == nil {
                    guard let url = voice.fileURL(in: store) else { throw VoiceError.noRecording }
                    let data = try Data(contentsOf: url)
                    let attachment = try await client.upload(data: data, name: voice.filename, mime: "audio/mp4")
                    guard self.client === client, draft.voice?.id == voice.id else { return }
                    draft.voice?.uploadedAttachment = attachment
                    request.attachmentIDs.append(attachment.id)
                    draft.pending = request
                    try store.save(draft, name: "draft.json")
                    cachedData[attachment.id] = data
                    if !content.files.contains(where: { $0.id == attachment.id }) { content.files.insert(attachment, at: 0) }
                    persistCache()
                }
                let message = try await client.send(request)
                guard self.client === client else { return }
                activityRevision += 1; activity = ServerActivity(state: "queued")
                content.merge(MessagePage(messages: [message])); persistCache()
                if draft.pending?.clientMessageID == request.clientMessageID {
                    if request.matches(text: draft.text, attachments: draft.sendAttachments, taskID: draft.taskID) { draft = Draft() }
                    else {
                        draft.pending = nil
                        draft.attachments.removeAll { originalAttachments.contains($0.id) }
                        if draft.voice?.id == voice?.id { draft.voice = nil }
                    }
                    if let voice, let url = voice.fileURL(in: store) { try? FileManager.default.removeItem(at: url) }
                    selectedDraftAttachmentID = nil
                }
                await refresh()
            } catch { guard self.client === client else { return }; self.error = error.localizedDescription }
        }
    }
    public func retry() {
        guard !modalPresented, !errorIsLocalVoice, !sending, KeyboardGate.permitsPrimaryAction() else { return }
        if draft.pending != nil && canSend { submit() }
        else { Task { await refresh(force: true) } }
    }
    public func stop() {
        guard !modalPresented, KeyboardGate.permitsPrimaryAction(), canStop else { return }
        guard let client else { return }
        let id = stopID ?? UUID().uuidString.lowercased(); stopID = id
        stopping = true; error = nil
        Task {
            defer { stopping = false }
            do { try await client.stop(id: id); guard self.client === client else { return }; stopID = nil; await refresh() }
            catch { guard self.client === client else { return }; self.error = error.localizedDescription }
        }
    }
    public func clearTaskContext() {
        guard !sending else { return }
        draft.taskID = nil; focusComposer += 1
    }
    public func escape() {
        if updates.phase == .installing { return }
        if showUpdates { closeUpdates() }
        else if preview != nil { preview = nil }
        else if showSettings { if !connecting { showSettings = false }; return }
        else if section == .conversation && (voiceBusy || draft.voice != nil) && !sending && !uploading { cancelVoice() }
        else if selectedTaskID != nil && section == .tasks { selectedTaskID = nil }
        else if draft.taskID != nil && !sending { clearTaskContext() }
        else if sidebarVisible { sidebarVisible = false }
    }
    public func attach() {
        guard !modalPresented, !uploading, !sending, !voiceBusy, client != nil, attachmentCount < 10 else { return }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false; panel.allowsMultipleSelection = true
        panel.prompt = "Прикрепить"
        guard let window = NSApp.keyWindow else { return }
        panel.beginSheetModal(for: window) { [weak self] response in
            guard response == .OK else { return }
            Task { @MainActor [weak self] in await self?.upload(panel.urls) }
        }
    }
    public func upload(_ urls: [URL]) async {
        guard let client, !uploading, !sending, !voiceBusy, !modalPresented else { return }
        uploading = true; error = nil
        defer { uploading = false }
        for url in urls.prefix(max(0, 10 - attachmentCount)) {
            do {
                let size = try url.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
                guard size <= APIClient.attachmentLimit else { throw APIError.tooLarge }
                let data = try Data(contentsOf: url)
                let mime = UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
                let attachment = try await client.upload(data: data, name: url.lastPathComponent, mime: mime)
                guard self.client === client else { return }
                draft.attachments.append(attachment)
                cachedData[attachment.id] = data
                if attachment.isImage, let image = NSImage(data: data) { images[attachment.id] = image }
                content.files.insert(attachment, at: 0); persistCache()
            } catch { guard self.client === client else { return }; self.error = error.localizedDescription; return }
        }
    }
    public var canRemoveDraftAttachment: Bool {
        section == .conversation && selectedDraftAttachmentID != nil
        && (draft.attachments.contains { $0.id == selectedDraftAttachmentID } || draft.voice?.id == selectedDraftAttachmentID)
        && !modalPresented && !voiceBusy && !sending && !uploading && updates.phase != .installing
    }
    public func openDraftAttachment(_ id: String? = nil) {
        guard section == .conversation, !modalPresented, KeyboardGate.permitsPrimaryAction(), !sending, !uploading, !voiceBusy,
              let attachment = draft.attachments.first(where: { $0.id == (id ?? selectedDraftAttachmentID) }) else { return }
        selectedDraftAttachmentID = attachment.id
        NSApp?.keyWindow?.makeFirstResponder(nil)
        open(attachment)
    }
    public func removeDraftAttachment(_ id: String? = nil) {
        guard section == .conversation, !modalPresented, KeyboardGate.permitsPrimaryAction(), !sending, !uploading, !voiceBusy,
              let target = id ?? selectedDraftAttachmentID,
              draft.attachments.contains(where: { $0.id == target }) || draft.voice?.id == target else { return }
        if draft.voice?.id == target { cancelVoice(); return }
        draft.attachments.removeAll { $0.id == target }
        if selectedDraftAttachmentID == target { selectedDraftAttachmentID = nil }
        draft.pending = nil
        focusComposer += 1
    }
    public var errorIsLocalVoice: Bool { voiceErrorMessage != nil && error == voiceErrorMessage }
    private func reportVoiceError(_ failure: Error) { voiceErrorMessage = failure.localizedDescription; error = voiceErrorMessage }
    private func clearVoiceError() { if errorIsLocalVoice { error = nil }; voiceErrorMessage = nil }
    public var voicePhase: VoicePhase { voiceActivity == .idle && draft.voice != nil ? .ready : voiceActivity }
    public var voiceBusy: Bool { voiceActivity == .recording || voiceActivity == .requestingPermission }
    public var attachmentCount: Int { draft.attachments.count + (draft.voice == nil ? 0 : 1) }
    public var canToggleRecording: Bool {
        section == .conversation && !modalPresented && !sending && !uploading && !connecting
        && updates.phase != .installing && (voiceBusy || (draft.voice == nil && attachmentCount < 10))
    }
    public var canPlayDraft: Bool {
        section == .conversation && draft.voice != nil && !voiceBusy && !sending && !uploading && !modalPresented && updates.phase != .installing
    }
    public func toggleRecording() {
        guard canToggleRecording, KeyboardGate.permitsPrimaryAction(), composer?.hasMarkedText() != true else { return }
        switch voiceActivity {
        case .requestingPermission: cancelVoice()
        case .recording: finishRecording()
        default:
            stopPlayback(); clearVoiceError(); error = nil
            let operation = UUID(); permissionID = operation
            voiceActivity = .requestingPermission
            Task { [self] in
                let granted = await voiceRecorder.requestPermission()
                guard permissionID == operation, voiceActivity == .requestingPermission else { return }
                permissionID = nil
                guard canToggleRecording, composer?.hasMarkedText() != true else { voiceActivity = .idle; return }
                guard granted else { voiceActivity = .idle; reportVoiceError(VoiceError.microphoneDenied); return }
                do {
                    let voice = VoiceDraft(durationSeconds: 0)
                    guard let url = voice.fileURL(in: store) else { throw VoiceError.noRecording }
                    try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
                    try voiceRecorder.start(at: url)
                    try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
                    draft.voice = voice; recordingID = voice.id
                    voiceActivity = .recording; recordingSeconds = 0
                    recordingTask = Task { [weak self] in
                        while !Task.isCancelled {
                            try? await Task.sleep(for: .milliseconds(100))
                            guard let self, self.voiceActivity == .recording else { return }
                            self.updateRecordingProgress()
                        }
                    }
                } catch {
                    _ = voiceRecorder.stop()
                    voiceActivity = .idle; recordingID = nil
                    reportVoiceError(error)
                }
            }
        }
    }
    public func updateRecordingProgress() {
        guard voiceActivity == .recording else { return }
        recordingSeconds = min(MacVoiceRecorder.maximumDuration, max(recordingSeconds, voiceRecorder.elapsed))
        if voiceRecorder.failure != nil || !voiceRecorder.isRecording || recordingSeconds >= MacVoiceRecorder.maximumDuration { finishRecording() }
    }
    public func finishRecording() {
        guard voiceActivity == .recording else { return }
        recordingTask?.cancel(); recordingTask = nil
        let failure = voiceRecorder.failure
        let duration = min(MacVoiceRecorder.maximumDuration, max(recordingSeconds, voiceRecorder.stop()))
        voiceActivity = .idle; recordingID = nil; recordingSeconds = 0
        guard let voice = draft.voice, let url = voice.fileURL(in: store) else { reportVoiceError(VoiceError.noRecording); return }
        do {
            let size = try url.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
            guard duration > 0, size > 0 else { throw VoiceError.noRecording }
            guard size <= APIClient.attachmentLimit else { throw APIError.tooLarge }
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
            draft.voice?.durationSeconds = duration
            selectedDraftAttachmentID = nil
            if let failure { reportVoiceError(failure) }
        } catch {
            // Keep a partial recording available for recovery; removing it is explicit.
            reportVoiceError(error)
            return
        }
        do { try store.save(draft, name: "draft.json") }
        catch { reportVoiceError(error) }
    }
    public func cancelVoice() {
        guard !modalPresented, !sending, !uploading, updates.phase != .installing,
              KeyboardGate.permitsPrimaryAction(), composer?.hasMarkedText() != true else { return }
        clearVoiceError()
        permissionID = nil
        if voiceActivity == .recording { _ = voiceRecorder.stop() }
        recordingTask?.cancel(); recordingTask = nil
        voiceActivity = .idle; recordingID = nil; recordingSeconds = 0
        stopPlayback()
        if let voice = draft.voice, let url = voice.fileURL(in: store) { try? FileManager.default.removeItem(at: url) }
        draft.voice = nil; draft.pending = nil
        selectedDraftAttachmentID = nil; focusComposer += 1
    }
    public func prepareVoiceForQuit() {
        permissionID = nil
        if voiceActivity == .requestingPermission { voiceActivity = .idle }
        if voiceActivity == .recording { finishRecording() }
        stopPlayback()
    }
    public func stopPlayback() {
        playbackID = nil; playbackTask?.cancel(); playbackTask = nil
        audioPlayer.stop(); playingAudioID = nil; loadingAudioID = nil
    }
    public func toggleDraftPlayback() {
        guard canPlayDraft, KeyboardGate.permitsPrimaryAction(), composer?.hasMarkedText() != true, let voice = draft.voice else { return }
        if playingAudioID == voice.id { stopPlayback(); return }
        stopPlayback(); clearVoiceError()
        do {
            guard let url = voice.fileURL(in: store) else { throw VoiceError.noRecording }
            try audioPlayer.play(url: url)
            selectedDraftAttachmentID = voice.id
            NSApp?.keyWindow?.makeFirstResponder(nil)
            playingAudioID = voice.id; monitorPlayback()
        } catch { reportVoiceError(error) }
    }
    public func playAttachment(_ attachment: Attachment) {
        guard attachment.isAudio, !modalPresented, !voiceBusy, !sending, !uploading,
              KeyboardGate.permitsPrimaryAction(), let client else { return }
        if playingAudioID == attachment.id || loadingAudioID == attachment.id { stopPlayback(); return }
        stopPlayback(); clearVoiceError()
        let operation = UUID(); playbackID = operation; loadingAudioID = attachment.id
        Task {
            do {
                let data: Data
                if let cached = cachedData[attachment.id] { data = cached }
                else { data = try await client.download(attachment) }
                guard self.client === client, playbackID == operation else { return }
                try audioPlayer.play(data: data)
                cachedData[attachment.id] = data
                loadingAudioID = nil; playingAudioID = attachment.id; monitorPlayback()
            } catch {
                guard self.client === client, playbackID == operation else { return }
                loadingAudioID = nil; reportVoiceError(error)
            }
        }
    }
    private func monitorPlayback() {
        playbackTask?.cancel()
        playbackTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(100))
                guard let self else { return }
                if !self.audioPlayer.isPlaying { self.playingAudioID = nil; return }
            }
        }
    }
    public func loadImage(_ attachment: Attachment) async {
        guard attachment.isImage, images[attachment.id] == nil, !imageLoads.contains(attachment.id), let client else { return }
        imageLoads.insert(attachment.id)
        defer { imageLoads.remove(attachment.id) }
        do {
            let data = try await client.download(attachment)
            guard self.client === client else { return }
            guard let image = NSImage(data: data) else { return }
            cachedData[attachment.id] = data; images[attachment.id] = image
        } catch { /* Download failures are shown when the user opens or saves the file. */ }
    }
    public func open(_ attachment: Attachment) {
        guard !modalPresented, !voiceBusy, updates.phase != .installing else { return }
        selectedFile = attachment
        if attachment.isImage { preview = attachment; Task { await loadImage(attachment) } }
        else if attachment.isAudio { playAttachment(attachment) }
        else { save(attachment) }
    }
    public func save(_ attachment: Attachment) {
        guard !downloading, !showSettings, let window = NSApp.keyWindow, window.attachedSheet == nil else { return }
        let panel = NSSavePanel(); panel.nameFieldStringValue = attachment.name; panel.prompt = "Сохранить"
        panel.beginSheetModal(for: window) { [weak self] response in
            guard response == .OK, let url = panel.url else { return }
            Task { @MainActor [weak self] in
                guard let self, let client = self.client else { return }
                self.downloading = true
                defer { self.downloading = false }
                do {
                    let data: Data
                    if let cached = self.cachedData[attachment.id] { data = cached }
                    else { data = try await client.download(attachment) }
                    guard self.client === client else { return }
                    try data.write(to: url, options: .atomic)
                } catch { guard self.client === client else { return }; self.error = error.localizedDescription }
            }
        }
    }
    public func moveImage(_ offset: Int) {
        guard let preview else { return }
        let all = draft.attachments.filter(\.isImage) + content.messages.flatMap(\.attachments).filter(\.isImage) + content.files.filter(\.isImage)
        var seen = Set<String>()
        let unique = all.filter { seen.insert($0.id).inserted }
        guard let index = unique.firstIndex(where: { $0.id == preview.id }), !unique.isEmpty else { return }
        let next = unique[(index + offset + unique.count) % unique.count]
        self.preview = next; selectedFile = next
        Task { await loadImage(next) }
    }
}
