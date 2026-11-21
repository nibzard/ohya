import Foundation

public enum FTSQueryFormatter {
    private static let explicitOperators: Set<String> = ["AND", "OR", "NOT", "NEAR"]

    public static func format(_ raw: String) -> String {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return "" }
        if trimmed.contains("\"") {
            return trimmed
        }
        let tokens = trimmed.split(whereSeparator: { $0.isWhitespace }).map(String.init)
        return tokens.map { token in
            if explicitOperators.contains(token) {
                return token
            }
            if tokenNeedsQuoting(token) {
                let escaped = token.replacingOccurrences(of: "\"", with: "\"\"")
                return "\"\(escaped)\""
            }
            return token
        }.joined(separator: " ")
    }

    private static func tokenNeedsQuoting(_ token: String) -> Bool {
        for scalar in token.unicodeScalars {
            if CharacterSet.alphanumerics.contains(scalar) || scalar == "_" || scalar == "*" {
                continue
            }
            return true
        }
        return false
    }
}

public enum HandoffPromptBuilder {
    public static func build(for session: SessionSummary) -> String {
        let scope = projectDisplayName(repo: session.repo, cwd: session.cwd)
        let activeNote: String
        if session.settled == false {
            activeNote = " This session was active in the last 10 minutes, so query events directly instead of events_settled."
        } else {
            activeNote = ""
        }
        return "Use ohya to inspect session \(session.id) (\(session.agent), \(scope)). Summarize what was decided, what was tried or failed, and what remains unfinished. Cite event IDs and verify against current files before continuing.\(activeNote)"
    }

    public static func build(for hit: SearchHit) -> String {
        let scope = projectDisplayName(repo: hit.repo, cwd: nil)
        return "Use ohya to inspect event \(hit.eventID) in session \(hit.sessionID) (\(hit.agent), \(scope)) and its surrounding context. Summarize the diagnosis, decisions, and remaining work, citing event IDs."
    }

    public static func projectDisplayName(repo: String?, cwd: String?) -> String {
        if let repo, !repo.isEmpty {
            let parts = repo.split(separator: "/")
            if parts.count >= 2 {
                return parts.suffix(2).joined(separator: "/")
            }
            return repo
        }
        if let cwd, !cwd.isEmpty {
            let name = URL(fileURLWithPath: cwd).lastPathComponent
            if !name.isEmpty {
                return name
            }
        }
        return "workspace"
    }
}

public enum MenuBarIconState: String, Sendable, Equatable {
    case caughtUp
    case syncing
    case degraded
    case paused
    case unavailable

    public var accessibilityLabel: String {
        switch self {
        case .caughtUp: return "ohya: caught up"
        case .syncing: return "ohya: syncing memory"
        case .degraded: return "ohya: attention needed"
        case .paused: return "ohya: capture paused"
        case .unavailable: return "ohya: CLI unavailable"
        }
    }

    public var shortStatusLabel: String {
        switch self {
        case .caughtUp: return "Caught up"
        case .syncing: return "Syncing"
        case .degraded: return "Attention"
        case .paused: return "Paused"
        case .unavailable: return "Unavailable"
        }
    }
}

public struct TransitionAlert: Sendable, Equatable {
    public let identifier: String
    public let title: String
    public let body: String
}

public struct StatusSummary: Sendable, Equatable {
    public let iconState: MenuBarIconState
    public let topologyLabel: String
    public let isHub: Bool
    public let isRemote: Bool
    public let totalBacklogBytes: Double
    public let stoppedSources: Int
    public let activeSources: Int
    public let warnings: [StatusFinding]
    public let isPaused: Bool

    public var actionableWarnings: [StatusFinding] {
        warnings.filter { !StatusEvaluator.backlogFindingCodes.contains($0.code) }
    }

    static func unavailable(_ topologyLabel: String) -> StatusSummary {
        StatusSummary(
            iconState: .unavailable,
            topologyLabel: topologyLabel,
            isHub: false,
            isRemote: false,
            totalBacklogBytes: 0,
            stoppedSources: 0,
            activeSources: 0,
            warnings: [],
            isPaused: false
        )
    }
}

public enum StatusEvaluator {
    static let backlogFindingCodes: Set<String> = ["pending_backlog", "upload_backlog"]

    public static func evaluate(status: StatusData?, error: OhyaRunnerError?) -> StatusSummary {
        guard let status else {
            return .unavailable(error == nil ? "Starting" : "Not connected")
        }

        let isRemote = status.topology.mode == "remote"
        let hasHubService = status.topology.services?.contains(where: { $0.name == "hub-service" }) ?? false
        let hasServeInstance = status.instances.contains(where: { $0.roles.contains("serve") })
        let isHub = !isRemote && (hasHubService || hasServeInstance)

        let topologyLabel: String
        if isRemote {
            topologyLabel = "Joined"
        } else if isHub {
            topologyLabel = "Hub"
        } else {
            topologyLabel = "Local"
        }

        var backlogBytes = 0.0
        var stopped = 0
        var active = 0
        for instance in status.instances {
            if let s = instance.sources.stopped {
                stopped += Int(s)
            }
            if let a = instance.sources.active {
                active += Int(a)
            }
            if let b = instance.backlog {
                backlogBytes += max(0, b.pendingCompleteBytes ?? 0)
                if let upload = b.uploadBytes {
                    for (_, value) in upload {
                        backlogBytes += max(0, value)
                    }
                }
            }
        }

        let warnings = status.findings.filter { $0.severity == "warn" || $0.severity == "error" }
        let servicePaused = status.topology.services?.contains(where: {
            $0.name == "service" && $0.loaded == false && $0.running == false
        }) ?? false

        let iconState: MenuBarIconState
        if error != nil {
            iconState = .unavailable
        } else if servicePaused && status.instances.allSatisfy({ $0.process != "running" }) {
            iconState = .paused
        } else if warnings.contains(where: { !backlogFindingCodes.contains($0.code) }) || stopped > 0 {
            iconState = .degraded
        } else if backlogBytes > 0 {
            iconState = .syncing
        } else {
            iconState = .caughtUp
        }

        return StatusSummary(
            iconState: iconState,
            topologyLabel: topologyLabel,
            isHub: isHub,
            isRemote: isRemote,
            totalBacklogBytes: backlogBytes,
            stoppedSources: stopped,
            activeSources: active,
            warnings: warnings,
            isPaused: servicePaused
        )
    }

    public static func transitionAlert(previous: StatusSummary?, current: StatusSummary) -> TransitionAlert? {
        guard let previous else { return nil }
        if current.stoppedSources > 0 && previous.stoppedSources == 0 {
            return TransitionAlert(
                identifier: "ohya.source.stopped",
                title: "ohya source stopped",
                body: "\(current.stoppedSources) session source stopped on a malformed or oversized record. Open ohya for the fix."
            )
        }
        let hadDown = previous.warnings.contains(where: { $0.code == "instance_terminated" || $0.code == "no_instances" })
        let hasDown = current.warnings.contains(where: { $0.code == "instance_terminated" || $0.code == "no_instances" })
        if hasDown && !hadDown && !current.isPaused {
            return TransitionAlert(
                identifier: "ohya.service.down",
                title: "ohya capture is not running",
                body: "The background memory process stopped unexpectedly. Open ohya to restart or inspect logs."
            )
        }
        return nil
    }
}

public enum DisplayFormat {
    public static func bytes(_ value: Double) -> String {
        if value < 1024 {
            return "\(Int(value)) B"
        }
        let units = ["KB", "MB", "GB", "TB"]
        var current = value / 1024.0
        var index = 0
        while current >= 1024.0 && index < units.count - 1 {
            current /= 1024.0
            index += 1
        }
        if current >= 100 {
            return String(format: "%.0f %@", current, units[index])
        }
        return String(format: "%.1f %@", current, units[index])
    }

    public static func relativeAge(fromEpochMS ms: Int64, nowMS: Int64 = Int64(Date().timeIntervalSince1970 * 1000)) -> String {
        let delta = max(0, (nowMS - ms) / 1000)
        if delta < 5 { return "just now" }
        if delta < 60 { return "\(delta)s ago" }
        let minutes = delta / 60
        if minutes < 60 { return "\(minutes)m ago" }
        let hours = minutes / 60
        if hours < 24 { return "\(hours)h ago" }
        let days = hours / 24
        return "\(days)d ago"
    }

    public static func shortID(_ id: String) -> String {
        if id.count <= 8 { return id }
        return String(id.prefix(8))
    }

    public static func agentDisplayName(_ agent: String) -> String {
        switch agent {
        case "claude-code": return "Claude Code"
        case "codex": return "Codex"
        case "pi": return "Pi"
        default: return agent
        }
    }

    public static func countdown(untilRFC3339 timestamp: String, now: Date = Date()) -> String? {
        let formatter = ISO8601DateFormatter()
        guard let target = formatter.date(from: timestamp) else { return nil }
        let remaining = Int(target.timeIntervalSince(now))
        if remaining <= 0 { return "Expired" }
        let mins = remaining / 60
        let secs = remaining % 60
        return String(format: "%d:%02d", mins, secs)
    }
}
