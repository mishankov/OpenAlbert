import Foundation
import XCTest
@testable import AlbertCore

final class CachePrecisionTests: XCTestCase {
    func testCachePreservesOrderOfMessagesWithinTheSameSecond() throws {
        var cache = CachedContent()
        let second = 1_793_000_000.0
        cache.messages = [
            Message(id: "telegram:1", role: "user", text: "Вопрос", createdAt: Date(timeIntervalSince1970: second + 0.125)),
            Message(id: "outgoing:1", role: "assistant", text: "Ответ", createdAt: Date(timeIntervalSince1970: second + 0.250)),
        ]
        let data = try APIJSON.encoder().encode(cache)
        var restored = try APIJSON.decoder().decode(CachedContent.self, from: data)
        restored.merge(MessagePage(messages: []))
        XCTAssertEqual(restored.messages.map(\.id), ["telegram:1", "outgoing:1"])
        XCTAssertGreaterThan(restored.messages[1].createdAt, restored.messages[0].createdAt)
    }
}
