import XCTest
@testable import AlbertCore

final class StubURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (Int, Data))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        do {
            let (status, data) = try Self.handler!(request)
            client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch { client?.urlProtocol(self, didFailWithError: error) }
    }
    override func stopLoading() {}
}

final class CoreTests: XCTestCase {
    func client() throws -> APIClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        return try APIClient(baseURL: URL(string: "https://albert.example")!, token: "private-token", session: URLSession(configuration: config))
    }
    override func tearDown() { StubURLProtocol.handler = nil }
    func testHTTPAndHTTPSAccepted() throws {
        for value in ["https://albert.example", "http://albert.example", "http://192.168.1.31:8080", "http://pi.local:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"] { XCTAssertNoThrow(try APIClient.validatedURL(value)) }
        for value in ["http://", "file:///tmp/", "https://user:token@albert.example", "https://albert.example?token=x", "https://albert.example/#secret", "https://albert.example/api"] { XCTAssertThrowsError(try APIClient.validatedURL(value), value) }
    }
    func testMessageRouteAuthorizationAndChronology() async throws {
        StubURLProtocol.handler = { request in
            XCTAssertEqual(request.url?.path, "/api/v1/messages")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer private-token")
            let parts = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)!
            XCTAssertEqual(parts.queryItems?.first { $0.name == "before" }?.value, "opaque/+?=")
            XCTAssertFalse(request.url!.absoluteString.contains("private-token"))
            return (200, Data(##"{"messages":[{"id":"a","role":"assistant","text":"# Ответ","format":"markdown","created_at":"2026-10-26T10:00:00.123456Z","delivery":"sent","attachments":[],"task_id":null}],"has_more":true,"next_before":"next"}"##.utf8))
        }
        let page = try await client().messages(before: "opaque/+?=")
        XCTAssertEqual(page.messages.first?.format, "markdown")
        XCTAssertEqual(page.nextBefore, "next")
    }
    func testSendUsesRetainedUUIDAndSnakeCaseContract() async throws {
        let request = SendRequest(clientMessageID: "97d9c4e8-5be4-4e3b-9c13-e10692f64918", text: "Привет", attachmentIDs: ["photo"], taskID: "task")
        StubURLProtocol.handler = { incoming in
            XCTAssertEqual(incoming.httpMethod, "POST")
            let body = try JSONSerialization.jsonObject(with: incoming.bodyForTests) as! [String: Any]
            XCTAssertEqual(body["client_message_id"] as? String, request.clientMessageID)
            XCTAssertEqual(body["attachment_ids"] as? [String], ["photo"])
            XCTAssertEqual(body["task_id"] as? String, "task")
            return (202, Data(##"{"message":{"id":"desktop:97d9c4e8-5be4-4e3b-9c13-e10692f64918","role":"user","text":"Привет","format":"plain","created_at":"2026-10-26T10:00:00Z","delivery":"accepted","attachments":[],"task_id":"task"}}"##.utf8))
        }
        let api = try client()
        let first = try await api.send(request)
        let repeated = try await api.send(request)
        XCTAssertEqual(first, repeated)
    }
    func testErrorIsActionableAndTokenDoesNotAppear() async throws {
        StubURLProtocol.handler = { _ in (401, Data(##"{"error":{"code":"unauthorized","message":"bad token"}}"##.utf8)) }
        do { _ = try await client().status(); XCTFail("must reject") }
        catch { XCTAssertEqual(error as? APIError, .unauthorized); XCTAssertFalse(error.localizedDescription.contains("private-token")) }
    }
    func testMultipartAndEscapedArtifactPath() async throws {
        StubURLProtocol.handler = { request in
            if request.httpMethod == "POST" {
                XCTAssertEqual(request.url?.path, "/api/v1/attachments")
                XCTAssertTrue(request.value(forHTTPHeaderField: "Content-Type")!.contains("multipart/form-data; boundary="))
                let body = String(decoding: request.bodyForTests, as: UTF8.self)
                XCTAssertTrue(body.contains("name=\"file\""))
                XCTAssertFalse(body.contains("filename=\"bad\r\n"))
                return (201, Data(##"{"attachment":{"id":"scope/a?b#c","name":"test.txt","mime":"text/plain","size_bytes":3}}"##.utf8))
            }
            XCTAssertEqual(request.url?.percentEncodedPathForTests, "/api/v1/attachments/scope%2Fa%3Fb%23c/content")
            return (200, Data("abc".utf8))
        }
        let api = try client()
        let file = try await api.upload(data: Data("abc".utf8), name: "bad\r\nname.txt", mime: "text/plain")
        let downloaded = try await api.download(file)
        XCTAssertEqual(downloaded, Data("abc".utf8))
    }
    func testOversizeRejectedBeforeNetwork() async throws {
        StubURLProtocol.handler = { _ in XCTFail("network must not run"); return (500, Data()) }
        do { _ = try await client().upload(data: Data(count: APIClient.attachmentLimit + 1), name: "huge.bin", mime: "application/octet-stream"); XCTFail("must reject") }
        catch { XCTAssertEqual(error as? APIError, .tooLarge) }
    }
    func testRedirectNeverForwardsAuthorization() {
        let delegate = NoRedirectDelegate()
        let session = URLSession(configuration: .ephemeral)
        let source = URL(string: "https://albert.example/api/v1/status")!
        let redirect = URLRequest(url: URL(string: "https://other.example")!)
        var chosen: URLRequest?
        delegate.urlSession(session, task: session.dataTask(with: source), willPerformHTTPRedirection: HTTPURLResponse(url: source, statusCode: 302, httpVersion: nil, headerFields: nil)!, newRequest: redirect) { chosen = $0 }
        XCTAssertNil(chosen)
    }
    func testDraftRetrySurvivesRestartAndNewPayloadUsesNewID() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let store = LocalStore(directory: directory)
        var draft = Draft(); draft.text = "Черновик"; draft.taskID = "task"
        let first = draft.request()
        try store.save(draft, name: "draft.json")
        var recovered = try XCTUnwrap(store.load(Draft.self, name: "draft.json"))
        XCTAssertEqual(recovered.request(), first)
        recovered.text += " изменён"
        XCTAssertNotEqual(recovered.request().clientMessageID, first.clientMessageID)
        let permissions = try FileManager.default.attributesOfItem(atPath: directory.appendingPathComponent("draft.json").path)[.posixPermissions] as? Int
        XCTAssertEqual(permissions, 0o600)
    }
    func testPollingMergeRetainsOlderPagesAndUpdatesDelivery() {
        var content = CachedContent()
        let old = Message(id: "old", role: "user", text: "Старое", createdAt: Date(timeIntervalSince1970: 1))
        let pending = Message(id: "new", role: "assistant", text: "Ответ", createdAt: Date(timeIntervalSince1970: 2), delivery: "pending")
        content.merge(MessagePage(messages: [old, pending]))
        var delivered = pending; delivered.delivery = "sent"
        content.merge(MessagePage(messages: [delivered]))
        XCTAssertEqual(content.messages.map(\.id), ["old", "new"])
        XCTAssertEqual(content.messages.last?.delivery, "sent")
    }
}

private extension URL {
    var percentEncodedPathForTests: String { URLComponents(url: self, resolvingAgainstBaseURL: false)!.percentEncodedPath }
}

private extension URLRequest {
    var bodyForTests: Data {
        if let httpBody { return httpBody }
        guard let stream = httpBodyStream else { return Data() }
        stream.open()
        defer { stream.close() }
        var data = Data(), buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let count = stream.read(&buffer, maxLength: buffer.count)
            if count <= 0 { break }
            data.append(buffer, count: count)
        }
        return data
    }
}
