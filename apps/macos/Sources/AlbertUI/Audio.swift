import AppKit
import AVFoundation

public enum VoicePhase: Equatable { case idle, requestingPermission, recording, ready }

public enum VoiceError: LocalizedError {
    case microphoneDenied, recordingFailed, noRecording, invalidRecording
    public var errorDescription: String? {
        switch self {
        case .microphoneDenied: return "Нет доступа к микрофону. Разрешите его в Системных настройках → Конфиденциальность и безопасность → Микрофон."
        case .recordingFailed: return "Не удалось начать запись. Проверьте микрофон."
        case .noRecording: return "Запись не сохранилась. Текст сообщения остался в черновике."
        case .invalidRecording: return "Не удалось открыть аудиозапись."
        }
    }
}

@MainActor
public protocol VoiceRecording: AnyObject {
    func requestPermission() async -> Bool
    func start(at url: URL) throws
    func stop() -> TimeInterval
    var elapsed: TimeInterval { get }
    var isRecording: Bool { get }
    var failure: Error? { get }
}

@MainActor
public protocol AudioPlaying: AnyObject {
    func play(url: URL) throws
    func play(data: Data) throws
    func stop()
    var isPlaying: Bool { get }
}

@MainActor
public final class MacVoiceRecorder: NSObject, VoiceRecording, AVAudioRecorderDelegate {
    public static let maximumDuration: TimeInterval = 300
    // Speech AAC, mono; shared with the offline encoder regression test.
    public static let settings: [String: Any] = [
        AVFormatIDKey: kAudioFormatMPEG4AAC,
        AVSampleRateKey: 16_000,
        AVNumberOfChannelsKey: 1,
        AVEncoderBitRateKey: 32_000,
        AVEncoderAudioQualityKey: AVAudioQuality.high.rawValue,
    ]
    private var recorder: AVAudioRecorder?
    public private(set) var failure: Error?
    public var elapsed: TimeInterval { recorder?.currentTime ?? 0 }
    public var isRecording: Bool { recorder?.isRecording == true }
    public func requestPermission() async -> Bool {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized: return true
        case .notDetermined: return await AVCaptureDevice.requestAccess(for: .audio)
        default: return false
        }
    }
    public func start(at url: URL) throws {
        failure = nil
        let recorder = try AVAudioRecorder(url: url, settings: Self.settings)
        recorder.delegate = self
        guard recorder.record(forDuration: Self.maximumDuration) else { throw VoiceError.recordingFailed }
        self.recorder = recorder
    }
    public func stop() -> TimeInterval {
        let duration = elapsed
        recorder?.stop(); recorder = nil
        return duration
    }
    nonisolated public func audioRecorderEncodeErrorDidOccur(_ recorder: AVAudioRecorder, error: Error?) {
        Task { @MainActor [weak self] in guard let self, self.recorder === recorder else { return }; self.failure = error ?? VoiceError.recordingFailed }
    }
    nonisolated public func audioRecorderDidFinishRecording(_ recorder: AVAudioRecorder, successfully flag: Bool) {
        if !flag { Task { @MainActor [weak self] in guard let self, self.recorder === recorder else { return }; self.failure = VoiceError.recordingFailed } }
    }
}

@MainActor
public final class MacAudioPlayer: AudioPlaying {
    private var player: AVAudioPlayer?
    public init() {}
    public var isPlaying: Bool { player?.isPlaying == true }
    public func play(url: URL) throws { try begin(AVAudioPlayer(contentsOf: url)) }
    public func play(data: Data) throws { try begin(AVAudioPlayer(data: data)) }
    private func begin(_ player: AVAudioPlayer) throws {
        self.player?.stop()
        guard player.prepareToPlay(), player.play() else { throw VoiceError.invalidRecording }
        self.player = player
    }
    public func stop() { player?.stop(); player = nil }
}

public enum AudioDuration {
    public static func string(_ seconds: TimeInterval) -> String {
        let total = Int(min(86_400, max(0, seconds.isFinite ? seconds : 0)))
        return String(format: "%d:%02d", total / 60, total % 60)
    }
}
