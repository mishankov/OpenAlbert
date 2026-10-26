import Foundation

/// Selects a desktop test backend without touching AppKit, credentials or user profiles.
public enum DesktopExecutionPolicy {
    public static let backendVariable = "ALBERT_DESKTOP_BACKEND"
    public static let runnerCommand = "python3 apps/macos/desktop_vm.py test"

    public enum Backend: Equatable, Sendable {
        case verifiedGuest
        case continuousIntegration
        case explicitHost
    }
    public enum Rejection: Equatable, Sendable {
        case virtualMachineRequired
        case unverifiedGuest
        case invalidBackend
    }
    public enum Decision: Equatable, Sendable {
        case allowed(Backend)
        case denied(Rejection)

        public var isAllowed: Bool { if case .allowed = self { return true }; return false }
        public var message: String {
            switch self {
            case .allowed: return "Desktop execution is allowed."
            case .denied(.unverifiedGuest):
                return "ALBERT_DESKTOP_BACKEND=guest requires a verified virtual machine (sysctl kern.hv_vmm_present must equal 1). Run \(DesktopExecutionPolicy.runnerCommand)."
            case .denied(.invalidBackend):
                return "Unknown ALBERT_DESKTOP_BACKEND. Run \(DesktopExecutionPolicy.runnerCommand) to use the desktop VM."
            case .denied(.virtualMachineRequired):
                return "Native desktop tests and snapshots require an isolated desktop VM. Run \(DesktopExecutionPolicy.runnerCommand). No host windows were opened. CI runners are supported; host execution requires an explicit operator opt-in with ALBERT_DESKTOP_BACKEND=host."
            }
        }
    }

    /// A guest marker is never sufficient by itself, even when CI is set.
    /// CI may select its own runner only when no explicit backend was requested.
    public static func evaluate(environment: [String: String], isVirtualMachine: Bool) -> Decision {
        let backend = environment[backendVariable]?.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() ?? ""
        switch backend {
        case "guest": return isVirtualMachine ? .allowed(.verifiedGuest) : .denied(.unverifiedGuest)
        case "host": return .allowed(.explicitHost)
        case "":
            if environment["CI"]?.lowercased() == "true" || environment["GITHUB_ACTIONS"]?.lowercased() == "true" {
                return .allowed(.continuousIntegration)
            }
            return .denied(.virtualMachineRequired)
        default: return .denied(.invalidBackend)
        }
    }
}
