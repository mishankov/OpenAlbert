import Foundation
import Darwin
import AlbertCore

/// This gate deliberately has no AppKit operations: call it before NSApplication.shared.
public enum DesktopIsolation {
    public struct Unavailable: LocalizedError {
        public let decision: DesktopExecutionPolicy.Decision
        public var errorDescription: String? { decision.message }
    }

    public static func isVirtualMachine() -> Bool {
        var value: Int32 = 0
        var size = MemoryLayout<Int32>.size
        return sysctlbyname("kern.hv_vmm_present", &value, &size, nil, 0) == 0 && value == 1
    }

    public static func requireAllowed(environment: [String: String] = ProcessInfo.processInfo.environment, virtualMachine: Bool? = nil) throws {
        let decision = DesktopExecutionPolicy.evaluate(environment: environment, isVirtualMachine: virtualMachine ?? isVirtualMachine())
        guard decision.isAllowed else { throw Unavailable(decision: decision) }
    }
}
