import AppKit
import SwiftUI
import AlbertCore

enum Palette {
    static let surface = Color(nsColor: .textBackgroundColor)
    static let quiet = Color(nsColor: NSColor(name: nil) { $0.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? NSColor(white: 0.14, alpha: 1) : NSColor(white: 0.96, alpha: 1) })
    static let line = Color.primary.opacity(0.09)
}

public struct RootView: View {
    @ObservedObject var model: AppModel
    public init(model: AppModel) { self.model = model }
    public var body: some View {
        HStack(spacing: 0) {
            if model.sidebarVisible { sidebar.frame(width: 180); Divider() }
            VStack(spacing: 0) {
                switch model.section {
                case .conversation: conversation
                case .tasks: tasks
                case .memory: memory
                case .files: files
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .background(Palette.surface)
        .sheet(isPresented: $model.showSettings) { SettingsView(model: model) }
        .sheet(isPresented: $model.showUpdates) { UpdatesView(updates: model.updates, model: model) }
        .sheet(item: $model.preview) { attachment in ImageViewer(model: model, attachment: attachment) }
        .onChange(of: model.section) { NSApp.mainMenu?.update() }
    }
    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 4) {
            ForEach(Section.allCases) { section in
                Button { model.navigate(section) } label: {
                    HStack { Text(section.title); Spacer() }.padding(.horizontal, 9).padding(.vertical, 8)
                        .background(model.section == section ? Color.primary.opacity(0.07) : .clear, in: RoundedRectangle(cornerRadius: 5))
                }.buttonStyle(.plain).font(.system(size: 12))
                    .help("\(section.title) · ⌘\(section.rawValue)")
                    .accessibilityIdentifier("section-\(section.rawValue)")
            }
            Spacer()
        }.padding(.horizontal, 12).padding(.top, 24).background(Palette.quiet)
    }
    private var conversation: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    VStack(alignment: .leading, spacing: 34) {
                        if model.nextBefore != nil {
                            Button(model.loadingOlder ? "Загрузка…" : "Раньше") { model.loadOlder() }
                                .buttonStyle(.plain).font(.system(size: 12)).foregroundStyle(.secondary)
                                .disabled(model.loadingOlder).help("Предыдущие сообщения · ⌃⌘↑")
                                .accessibilityIdentifier("olderMessages")
                        }
                        if model.content.messages.isEmpty {
                            if model.client == nil {
                                Button("Подключить Альберта") { model.openSettings() }
                                    .buttonStyle(.plain).disabled(!model.canOpenSettings).font(.system(size: 14)).foregroundStyle(.secondary)
                                    .help("Настройки подключения · ⌘,")
                            }
                        }
                        ForEach(model.content.messages) { message in MessageView(model: model, message: message).id(message.id) }
                        Color.clear.frame(height: 1).id("bottom")
                    }
                    .padding(.horizontal, 36).padding(.top, 54).padding(.bottom, 24)
                    .frame(maxWidth: 680, alignment: .leading).frame(maxWidth: .infinity)
                }
                .onAppear { proxy.scrollTo("bottom", anchor: .bottom) }
                .onChange(of: model.content.messages.last?.id) { proxy.scrollTo("bottom", anchor: .bottom) }
            }
            composer
        }
    }
    private var composer: some View {
        VStack(spacing: 9) {
            if let label = model.activityLabel {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small).scaleEffect(0.7).frame(width: 14, height: 14)
                    Text(label).font(.system(size: 12)).foregroundStyle(.secondary).lineLimit(2)
                    Spacer(minLength: 0)
                }
                .accessibilityElement(children: .combine)
                .accessibilityIdentifier("conversationActivity")
                .padding(.bottom, 3)
            }
            if let id = model.draft.taskID {
                HStack {
                    Button(model.contextTask?.title ?? id) { model.selectedTaskID = id; model.navigate(.tasks) }
                        .buttonStyle(.plain).font(.system(size: 12)).foregroundStyle(.secondary)
                    Spacer()
                    Button { model.clearTaskContext() } label: { Image(systemName: "xmark").font(.system(size: 10)) }
                        .buttonStyle(.plain).help("Убрать поручение · Esc").accessibilityLabel("Убрать поручение")
                        .disabled(model.sending)
                }
            }
            if !model.draft.attachments.isEmpty { DraftAttachmentsView(model: model) }
            if model.voicePhase != .idle { VoiceDraftView(model: model) }
            HStack(alignment: .bottom, spacing: 10) {
                Button { model.attach() } label: { Image(systemName: "plus").font(.system(size: 15)).frame(width: 24, height: 30) }
                    .buttonStyle(.plain).foregroundStyle(.secondary).help("Прикрепить файл · ⌘O").accessibilityLabel("Прикрепить файл")
                    .disabled(model.client == nil || model.uploading || model.sending || model.attachmentCount >= 10 || model.voiceBusy)
                ComposerEditor(text: $model.draft.text, model: model).frame(height: model.composerHeight)
                    .overlay(alignment: .topLeading) {
                        if model.draft.text.isEmpty { Text("Сообщение").font(.system(size: 14)).foregroundStyle(.tertiary).padding(.top, 8).allowsHitTesting(false) }
                    }
                if model.voicePhase == .idle {
                    Button { model.toggleRecording() } label: { Image(systemName: "mic").font(.system(size: 14)).frame(width: 24, height: 30) }
                        .buttonStyle(.plain).foregroundStyle(.secondary).help("Записать голосовое · ⌘⇧R").accessibilityLabel("Записать голосовое")
                        .accessibilityIdentifier("recordVoice").disabled(!model.canToggleRecording)
                }
                Button { model.submit() } label: {
                    HStack(spacing: 6) { Text("↵").font(.system(size: 10)).foregroundStyle(.secondary); Image(systemName: "arrow.up").font(.system(size: 16)) }.frame(height: 30)
                }.buttonStyle(.plain).help("Отправить · Enter. Новая строка · Shift+Enter").accessibilityLabel("Отправить")
                    .accessibilityIdentifier("sendMessage").disabled(!model.canSend)
                if model.awaitingResponse || model.stopping {
                    Button { model.stop() } label: { Image(systemName: "stop.fill").font(.system(size: 11)).frame(width: 24, height: 30) }
                        .buttonStyle(.plain).help("Остановить · ⌘.").accessibilityLabel("Остановить")
                        .disabled(!model.canStop)
                }
            }.padding(.vertical, 6).padding(.leading, 9).padding(.trailing, 12).background(Palette.quiet, in: RoundedRectangle(cornerRadius: 10))
            if let error = model.error {
                HStack(alignment: .firstTextBaseline, spacing: 9) {
                    Text(error).textSelection(.enabled)
                    Spacer(minLength: 8)
                    if !model.errorIsLocalVoice {
                        Button("Повторить") { model.retry() }
                            .buttonStyle(.plain).underline().help("Повторить · ⌘R")
                    }
                    if !model.connected { Button("Настройки") { model.openSettings() }.buttonStyle(.plain).disabled(!model.canOpenSettings).help("Подключение · ⌘,") }
                }.font(.system(size: 11)).foregroundStyle(.secondary)
            } else if model.uploading { Text("Загрузка файла…").font(.system(size: 11)).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading) }
        }.padding(.horizontal, 36).padding(.top, 12).padding(.bottom, 28)
            .frame(maxWidth: 680).frame(maxWidth: .infinity)
    }
    private var tasks: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                if let task = model.selectedTask {
                    Button { model.selectedTaskID = nil } label: { Text("← Поручения").font(.system(size: 12)).foregroundStyle(.secondary) }
                        .buttonStyle(.plain).help("Назад · Esc").padding(.bottom, 26)
                    Text(task.title).font(.system(size: 22, weight: .medium)).padding(.bottom, 9)
                    Text(task.statusLabel).font(.system(size: 12)).foregroundStyle(.secondary).padding(.bottom, 28)
                    MarkdownView(text: task.goal).font(.system(size: 14))
                    if let reason = task.reason, !reason.isEmpty { detail("Сейчас", reason) }
                    if let interval = task.scheduleSeconds, interval > 0 {
                        detail("Расписание", "Каждые \(Duration.seconds(interval).formatted(.units(allowed: [.hours, .minutes, .seconds], width: .wide)))")
                    }
                    if let next = task.nextRunAt, next.timeIntervalSince1970 > 0 {
                        Text("Следующий запуск: \(next.formatted(date: .abbreviated, time: .shortened))").font(.system(size: 12)).foregroundStyle(.secondary).padding(.top, 12)
                    }
                    if let result = task.result, !result.isEmpty { detail("Результат", result) }
                    Button { model.discuss() } label: { HStack(spacing: 18) { Text("Обсудить"); Text("⌘↵").foregroundStyle(.secondary) }.font(.system(size: 12)).padding(.horizontal, 12).padding(.vertical, 8).background(Palette.quiet, in: RoundedRectangle(cornerRadius: 6)) }
                        .buttonStyle(.plain).help("Обсудить поручение · ⌘Return").accessibilityIdentifier("discussTask")
                        .disabled(model.sending).padding(.top, 30)
                } else {
                    pageTitle("Поручения")
                    if model.content.tasks.isEmpty { empty("Поручений пока нет.") }
                    ForEach(model.content.tasks.filter { !$0.isFinished }) { taskRow($0) }
                    let finished = model.content.tasks.filter(\.isFinished)
                    if !finished.isEmpty {
                        Text("Завершено").font(.system(size: 12)).foregroundStyle(.secondary).padding(.top, 34).padding(.bottom, 12)
                        ForEach(finished) { taskRow($0) }
                    }
                }
                errorNotice
            }.padding(.horizontal, 36).padding(.vertical, 46).frame(maxWidth: 680, alignment: .leading).frame(maxWidth: .infinity)
        }
    }
    private func taskRow(_ task: AlbertTask) -> some View {
        Button { model.selectedTaskID = task.id } label: {
            VStack(alignment: .leading, spacing: 6) {
                Text(task.title).font(.system(size: 15, weight: .medium)).foregroundStyle(.primary)
                Text([task.statusLabel, task.reason].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.system(size: 12)).foregroundStyle(.secondary).lineLimit(2)
            }.frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 20).overlay(alignment: .top) { Palette.line.frame(height: 1) }
        }.buttonStyle(.plain).accessibilityIdentifier("task-\(task.id)")
    }
    private var memory: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                pageTitle("Память")
                if model.content.memory.isEmpty { empty("В памяти пока ничего нет.") }
                ForEach(model.content.memory) { entry in
                    VStack(alignment: .leading, spacing: 12) {
                        Text(entry.path).font(.system(size: 12)).foregroundStyle(.secondary)
                        MarkdownView(text: entry.content).font(.system(size: 14))
                    }.padding(.vertical, 20).frame(maxWidth: .infinity, alignment: .leading).overlay(alignment: .top) { Palette.line.frame(height: 1) }
                }
                errorNotice
            }.padding(.horizontal, 36).padding(.vertical, 46).frame(maxWidth: 680, alignment: .leading).frame(maxWidth: .infinity)
        }
    }
    private var files: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                pageTitle("Файлы")
                if model.content.files.isEmpty { empty("Файлов пока нет.") }
                ForEach(model.content.files) { attachment in
                    Button { model.open(attachment) } label: {
                        HStack(spacing: 12) {
                            if attachment.isImage { AttachmentImage(model: model, attachment: attachment).frame(width: 66, height: 46).clipped().clipShape(RoundedRectangle(cornerRadius: 4)) }
                            else { Image(systemName: "doc").font(.system(size: 23)).foregroundStyle(.secondary).frame(width: 66, height: 46) }
                            VStack(alignment: .leading, spacing: 5) {
                                Text(attachment.name).font(.system(size: 14)).lineLimit(2)
                                Text(ByteCountFormatter.string(fromByteCount: Int64(attachment.sizeBytes), countStyle: .file)).font(.system(size: 11)).foregroundStyle(.secondary)
                            }
                            Spacer()
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 16).overlay(alignment: .top) { Palette.line.frame(height: 1) }
                    }.buttonStyle(.plain).help("Открыть \(attachment.name). Сохранить выбранный файл: ⌘S")
                }
                errorNotice
            }.padding(.horizontal, 36).padding(.vertical, 46).frame(maxWidth: 680, alignment: .leading).frame(maxWidth: .infinity)
        }
    }
    private func pageTitle(_ text: String) -> some View { Text(text).font(.system(size: 20, weight: .medium)).padding(.bottom, 28) }
    private func empty(_ text: String) -> some View { Text(text).font(.system(size: 14)).foregroundStyle(.secondary) }
    private func detail(_ title: String, _ text: String) -> some View { VStack(alignment: .leading, spacing: 9) { Text(title).font(.system(size: 14, weight: .medium)); MarkdownView(text: text).font(.system(size: 14)) }.padding(.top, 28) }
    @ViewBuilder private var errorNotice: some View {
        if let error = model.error { Text(error).font(.system(size: 11)).foregroundStyle(.secondary).padding(.top, 25) }
    }
}

private struct MessageView: View {
    @ObservedObject var model: AppModel
    let message: Message
    var body: some View {
        HStack {
            if message.role == "user" { Spacer(minLength: 30) }
            VStack(alignment: .leading, spacing: 16) {
                if let taskID = message.taskID {
                    Button(model.content.tasks.first { $0.id == taskID }?.title ?? taskID) { model.selectedTaskID = taskID; model.navigate(.tasks) }
                        .buttonStyle(.plain).font(.system(size: 12)).foregroundStyle(.secondary).underline()
                }
                if !message.text.isEmpty {
                    if message.format == "markdown" { MarkdownView(text: message.text) }
                    else { Text(message.text).textSelection(.enabled).lineSpacing(6).fixedSize(horizontal: false, vertical: true) }
                }
                if !message.attachments.isEmpty {
                    LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 9), count: message.attachments.count == 1 ? 1 : 2), alignment: .leading, spacing: 9) {
                        ForEach(message.attachments) { attachment in
                            Button { model.open(attachment) } label: {
                                if attachment.isImage { AttachmentImage(model: model, attachment: attachment).clipShape(RoundedRectangle(cornerRadius: 7)) }
                                else { Label(attachment.isAudio ? (model.playingAudioID == attachment.id ? "Остановить" : model.loadingAudioID == attachment.id ? "Загрузка…" : "Голосовое сообщение") : attachment.name, systemImage: attachment.isAudio ? (model.playingAudioID == attachment.id ? "stop.fill" : "play.fill") : "doc").font(.system(size: 12)).lineLimit(2).padding(12).frame(maxWidth: .infinity, alignment: .leading).background(Palette.quiet, in: RoundedRectangle(cornerRadius: 6)) }
                            }.buttonStyle(.plain).help("Открыть \(attachment.name)").accessibilityLabel("Открыть \(attachment.name)")
                        }
                    }
                }
                if message.delivery == "failed" || message.delivery == "unknown" {
                    Text(message.delivery == "failed" ? "Не доставлено" : "Доставка не подтверждена").font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }
            .font(.system(size: message.role == "user" ? 14 : 15))
            .padding(message.role == "user" ? 14 : 0)
            .background(message.role == "user" ? Palette.quiet : .clear, in: RoundedRectangle(cornerRadius: 11))
            .frame(maxWidth: .infinity, alignment: .leading)
            if message.role != "user" { Spacer(minLength: 0) }
        }.accessibilityElement(children: .contain).accessibilityLabel(message.role == "user" ? "Вы" : "Альберт")
    }
}

struct AttachmentImage: View {
    @ObservedObject var model: AppModel
    let attachment: Attachment
    var body: some View {
        Group {
            if let image = model.images[attachment.id] { Image(nsImage: image).resizable().aspectRatio(contentMode: .fit) }
            else { VStack(spacing: 8) { Image(systemName: "photo"); Text(attachment.name).font(.system(size: 11)).lineLimit(2) }.foregroundStyle(.secondary).frame(maxWidth: .infinity, minHeight: 90).background(Palette.quiet) }
        }.task(id: attachment.id) { await model.loadImage(attachment) }
    }
}

private struct ImageViewer: View {
    @ObservedObject var model: AppModel
    let attachment: Attachment
    var body: some View {
        VStack(spacing: 16) {
            HStack {
                Text(attachment.name).font(.system(size: 12)).lineLimit(1)
                Spacer()
                Button("Сохранить") { model.save(attachment) }.buttonStyle(.plain).help("Сохранить файл · ⌘S").disabled(model.downloading)
                Button("Закрыть") { model.preview = nil }.buttonStyle(.plain).keyboardShortcut(.cancelAction).help("Закрыть · Esc")
            }.font(.system(size: 12))
            AttachmentImage(model: model, attachment: attachment).frame(maxWidth: .infinity, maxHeight: .infinity)
            if model.images[attachment.id] == nil { Text("Изображение недоступно. Попробуйте сохранить файл.").font(.system(size: 11)).foregroundStyle(.secondary) }
            HStack {
                Button { model.moveImage(-1) } label: { Image(systemName: "arrow.left") }.keyboardShortcut(.leftArrow, modifiers: []).help("Предыдущее изображение · ←").accessibilityLabel("Предыдущее изображение")
                Spacer()
                Button { model.moveImage(1) } label: { Image(systemName: "arrow.right") }.keyboardShortcut(.rightArrow, modifiers: []).help("Следующее изображение · →").accessibilityLabel("Следующее изображение")
            }.buttonStyle(.plain)
        }.padding(20).frame(minWidth: 620, idealWidth: 850, maxWidth: 1100, minHeight: 440, idealHeight: 620, maxHeight: 800).background(Palette.surface)
    }
}

private struct SettingsView: View {
    @ObservedObject var model: AppModel
    @State private var url = ""
    @State private var token = ""
    @State private var hasSavedToken = false
    @FocusState private var focusURL: Bool
    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            Text("Подключение").font(.system(size: 20, weight: .medium))
            VStack(alignment: .leading, spacing: 7) {
                Text("Сервер").font(.system(size: 12)).foregroundStyle(.secondary)
                TextField("https://albert.example.com", text: $url).textFieldStyle(.roundedBorder).focused($focusURL).accessibilityIdentifier("serverURL")
            }
            VStack(alignment: .leading, spacing: 7) {
                Text("Токен").font(.system(size: 12)).foregroundStyle(.secondary)
                SecureField(hasSavedToken ? "Сохранён в Связке ключей" : "Токен доступа", text: $token).textFieldStyle(.roundedBorder).accessibilityIdentifier("serverToken")
            }
            if let error = model.error { Text(error).font(.system(size: 12)).foregroundStyle(.secondary).textSelection(.enabled) }
            HStack {
                Button("Отмена") { model.showSettings = false }.keyboardShortcut(.cancelAction).disabled(model.connecting)
                Spacer()
                Button(model.connecting ? "Подключение…" : "Подключить") {
                    guard KeyboardGate.permitsPrimaryAction() else { return }
                    let saved = (try? APIClient.validatedURL(url)).flatMap { try? Keychain.token(for: $0.absoluteString) }
                    Task { await model.connect(url: url, token: token.isEmpty ? saved ?? "" : token) }
                }.keyboardShortcut(.defaultAction).disabled(model.connecting || (try? APIClient.validatedURL(url)) == nil || (token.isEmpty && !hasSavedToken))
                    .help("Подключить · Return").accessibilityIdentifier("connectServer")
            }
        }.padding(28).frame(width: 400)
            .onAppear { url = model.serverURL; hasSavedToken = (try? Keychain.token(for: model.serverURL)) != nil; focusURL = true }
            .onChange(of: url) { _, value in hasSavedToken = (try? APIClient.validatedURL(value)).flatMap { try? Keychain.token(for: $0.absoluteString) } != nil }
            .interactiveDismissDisabled(model.connecting)
    }
}

private struct DraftAttachmentsView: View {
    @ObservedObject var model: AppModel
    var body: some View {
        ScrollView(.horizontal) {
            HStack(spacing: 10) {
                ForEach(model.draft.attachments) { attachment in
                    ZStack(alignment: .topTrailing) {
                        Button { model.openDraftAttachment(attachment.id) } label: {
                            Group {
                                if attachment.isImage {
                                    AttachmentImage(model: model, attachment: attachment).frame(width: 84, height: 70)
                                } else {
                                    VStack(spacing: 7) {
                                        Image(systemName: attachment.isAudio ? "waveform" : "doc").font(.system(size: 19))
                                        Text(attachment.name).font(.system(size: 10)).lineLimit(1)
                                    }.frame(width: 84, height: 70).foregroundStyle(.secondary)
                                }
                            }.background(Palette.quiet).clipShape(RoundedRectangle(cornerRadius: 6))
                                .overlay { RoundedRectangle(cornerRadius: 6).strokeBorder(model.selectedDraftAttachmentID == attachment.id ? Color.primary.opacity(0.4) : Palette.line, lineWidth: 1) }
                        }.buttonStyle(.plain).help("Открыть \(attachment.name) · ⌘⇧O")
                            .accessibilityLabel("Открыть \(attachment.name)").accessibilityIdentifier("draft-attachment-\(attachment.id)")
                            .disabled(model.sending || model.uploading || model.voiceBusy)
                        Button { model.removeDraftAttachment(attachment.id) } label: {
                            Image(systemName: "xmark").font(.system(size: 8, weight: .medium)).frame(width: 18, height: 18)
                                .background(Palette.surface.opacity(0.95), in: Circle())
                        }.buttonStyle(.plain).padding(3).help("Убрать \(attachment.name) · ⌘⌫ после выбора")
                            .accessibilityLabel("Убрать \(attachment.name)").disabled(model.sending || model.uploading || model.voiceBusy)
                    }
                }
            }.padding(.vertical, 2)
        }.scrollIndicators(.hidden).frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct VoiceDraftView: View {
    @ObservedObject var model: AppModel
    var body: some View {
        HStack(spacing: 9) {
            if model.voicePhase == .requestingPermission {
                Text("Доступ к микрофону…").font(.system(size: 12)).foregroundStyle(.secondary)
            } else if model.voicePhase == .recording {
                Circle().fill(Color.red.opacity(0.8)).frame(width: 5, height: 5)
                Text(AudioDuration.string(model.recordingSeconds)).font(.system(size: 12).monospacedDigit())
                Button { model.toggleRecording() } label: { Image(systemName: "stop.fill").font(.system(size: 10)).frame(width: 24, height: 24) }
                    .buttonStyle(.plain).help("Закончить запись · ⌘⇧R").accessibilityLabel("Закончить запись")
                    .accessibilityIdentifier("finishVoice").disabled(!model.canToggleRecording)
            } else if let voice = model.draft.voice {
                Button { model.toggleDraftPlayback() } label: { Image(systemName: model.playingAudioID == voice.id ? "stop.fill" : "play.fill").font(.system(size: 11)).frame(width: 24, height: 24) }
                    .buttonStyle(.plain).help("Прослушать / остановить · ⌘⇧P").accessibilityLabel(model.playingAudioID == voice.id ? "Остановить прослушивание" : "Прослушать запись")
                    .accessibilityIdentifier("playDraftVoice").disabled(!model.canPlayDraft)
                Text(AudioDuration.string(voice.durationSeconds)).font(.system(size: 12).monospacedDigit()).foregroundStyle(.secondary)
            }
            Spacer()
            Button { model.cancelVoice() } label: { Image(systemName: "xmark").font(.system(size: 10)).frame(width: 24, height: 24) }
                .buttonStyle(.plain).help("Убрать запись · Esc").accessibilityLabel("Убрать запись")
                .accessibilityIdentifier("cancelVoice").disabled(model.sending || model.uploading)
        }.padding(.horizontal, 10).padding(.vertical, 5).background(Palette.quiet, in: RoundedRectangle(cornerRadius: 7))
    }
}
