import Foundation
import AVFAudio
import XCTest
@testable import AlbertCore

/// Optional cross-language test. The launcher starts real Go HTTP handlers with
/// disposable in-memory data; no Telegram/LLM or production credentials are used.
final class LiveAPIContractTests: XCTestCase {
    func testRealGoAPIContract() async throws {
        guard let address = ProcessInfo.processInfo.environment["ALBERT_CONTRACT_URL"] else {
            throw XCTSkip("Run python3 tools/checks/check-contract.py for the Go ↔ Swift test")
        }
        let client = try APIClient(baseURL: XCTUnwrap(URL(string: address)), token: "contract-test-only-not-a-real-owner-token")
        let status = try await client.status()
        XCTAssertEqual(status.apiVersion, 1)
        XCTAssertEqual(status.conversationID, "personal:123")
        XCTAssertEqual(status.maxAttachmentBytes, APIClient.attachmentLimit)
        let initial = try await client.messages()
        XCTAssertEqual(initial.messages.count, 2)
        XCTAssertFalse(initial.messages.contains { $0.text.contains("FOREIGN_SECRET") })
        XCTAssertEqual(initial.messages.last?.format, "markdown")
        XCTAssertTrue(initial.messages.last?.text.contains("```go") == true)
        let tasks = try await client.tasks()
        XCTAssertEqual(tasks.first?.id, "task-fixture")
        let memory = try await client.memory()
        XCTAssertEqual(memory.first?.path, "preferences.md")
        let initialFiles = try await client.files()
        let opaqueFile = try XCTUnwrap(initialFiles.first { $0.id == "folder/result #1?.txt" })
        let opaqueContents = try await client.download(opaqueFile)
        XCTAssertEqual(opaqueContents, Data("Opaque artifact ID".utf8))
        let bytes = Data("Вложение из Swift\n".utf8)
        let attachment = try await client.upload(data: bytes, name: "заметка.txt", mime: "text/plain")
        XCTAssertEqual(attachment.sizeBytes, bytes.count)
        let downloaded = try await client.download(attachment)
        XCTAssertEqual(downloaded, bytes)
        let request = SendRequest(text: "Обсудим поручение", attachmentIDs: [attachment.id], taskID: "task-fixture")
        let accepted = try await client.send(request)
        let retry = try await client.send(request)
        XCTAssertEqual(accepted.id, retry.id)
        XCTAssertEqual(accepted.taskID, "task-fixture")
        XCTAssertEqual(accepted.attachments.first?.id, attachment.id)
        var conflict = request
        conflict.text = "Другой текст"
        do {
            _ = try await client.send(conflict)
            XCTFail("Reusing a UUID with different content must fail")
        } catch APIError.server(let code, _) {
            XCTAssertEqual(code, 409)
        }
        let messages = try await client.messages()
        XCTAssertEqual(messages.messages.filter { $0.id == accepted.id }.count, 1)
        let files = try await client.files()
        XCTAssertTrue(files.contains { $0.id == attachment.id })
        // Encode actual AAC without requesting a microphone or using an audio output.
        // This catches MP4 container sniffing that would bypass the server's STT path.
        let voiceBytes = try silentM4A()
        let voice = try await client.upload(data: voiceBytes, name: "voice-contract.m4a", mime: "audio/mp4")
        XCTAssertEqual(voice.mime, "audio/mp4")
        let voiceRequest = SendRequest(text: "", attachmentIDs: [voice.id])
        let voiceAccepted = try await client.send(voiceRequest)
        let voiceRetried = try await client.send(voiceRequest)
        XCTAssertEqual(voiceAccepted.id, voiceRetried.id)
        XCTAssertEqual(voiceAccepted.attachments.first?.mime, "audio/mp4")
        XCTAssertTrue(voiceAccepted.text.isEmpty)
        let voiceDownloaded = try await client.download(voice)
        XCTAssertEqual(voiceDownloaded, voiceBytes)
        let stopID = UUID().uuidString.lowercased()
        try await client.stop(id: stopID)
        try await client.stop(id: stopID)
        let unauthorized = try APIClient(baseURL: XCTUnwrap(URL(string: address)), token: "wrong")
        do {
            _ = try await unauthorized.status()
            XCTFail("A wrong token must fail")
        } catch APIError.unauthorized { }
    }

    private func silentM4A() throws -> Data {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".m4a")
        defer { try? FileManager.default.removeItem(at: url) }
        func encode() throws {
            let file = try AVAudioFile(forWriting: url, settings: [AVFormatIDKey: kAudioFormatMPEG4AAC,
                AVSampleRateKey: 16000, AVNumberOfChannelsKey: 1, AVEncoderBitRateKey: 32000],
                commonFormat: .pcmFormatFloat32, interleaved: false)
            let buffer = try XCTUnwrap(AVAudioPCMBuffer(pcmFormat: file.processingFormat, frameCapacity: 1600))
            buffer.frameLength = 1600
            try XCTUnwrap(buffer.floatChannelData)[0].initialize(repeating: 0, count: 1600)
            try file.write(from: buffer)
        }
        try encode()
        return try Data(contentsOf: url)
    }
}
