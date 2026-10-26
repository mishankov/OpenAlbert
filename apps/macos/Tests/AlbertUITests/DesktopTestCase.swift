import XCTest
import AlbertUI

/// Every native test must pass the backend gate before its body can create NSApplication.
class DesktopTestCase: XCTestCase {
    override func setUpWithError() throws {
        try super.setUpWithError()
        try DesktopIsolation.requireAllowed()
    }
}
