import XCTest
@testable import AlbertCore

final class DesktopExecutionPolicyTests: XCTestCase {
    func testLocalDesktopDefaultHardFailsEvenInsideAnUnselectedVM() {
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: [:], isVirtualMachine: false), .denied(.virtualMachineRequired))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: [:], isVirtualMachine: true), .denied(.virtualMachineRequired))
        XCTAssertFalse(DesktopExecutionPolicy.evaluate(environment: [:], isVirtualMachine: false).isAllowed)
    }
    func testGuestMarkerRequiresVerifiedHypervisor() {
        let environment = ["ALBERT_DESKTOP_BACKEND": "guest"]
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: environment, isVirtualMachine: false), .denied(.unverifiedGuest))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: environment, isVirtualMachine: true), .allowed(.verifiedGuest))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: environment.merging(["CI": "true"]) { _, last in last }, isVirtualMachine: false), .denied(.unverifiedGuest))
    }
    func testCIUsesItsRunnerWithoutAHostOptIn() {
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["CI": "true"], isVirtualMachine: false), .allowed(.continuousIntegration))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["GITHUB_ACTIONS": "true"], isVirtualMachine: false), .allowed(.continuousIntegration))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["CI": "false", "GITHUB_ACTIONS": "false"], isVirtualMachine: false), .denied(.virtualMachineRequired))
    }
    func testHostRequiresAnExplicitBackendValue() {
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["ALBERT_DESKTOP_BACKEND": "host"], isVirtualMachine: false), .allowed(.explicitHost))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["ALBERT_DESKTOP_BACKEND": "auto"], isVirtualMachine: false), .denied(.invalidBackend))
        XCTAssertEqual(DesktopExecutionPolicy.evaluate(environment: ["ALBERT_DESKTOP_BACKEND": "unknown", "CI": "true"], isVirtualMachine: true), .denied(.invalidBackend))
    }
    func testRejectionsExplainHowToRunTheVMInsteadOfSkipping() {
        for reason in [DesktopExecutionPolicy.Rejection.virtualMachineRequired, .unverifiedGuest, .invalidBackend] {
            let decision = DesktopExecutionPolicy.Decision.denied(reason)
            XCTAssertTrue(decision.message.contains("python3 apps/macos/desktop_vm.py test"))
            XCTAssertFalse(decision.isAllowed)
        }
    }
}
