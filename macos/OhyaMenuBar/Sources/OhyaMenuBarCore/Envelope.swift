import Foundation

public struct JSONEnvelope<T: Decodable & Sendable>: Decodable, Sendable {
    public let schemaVersion: Int
    public let data: T?
    public let error: CLIErrorPayload?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case data
        case error
    }
}

public struct CLIErrorPayload: Decodable, Sendable, Equatable {
    public let code: String
    public let message: String
    public let hint: String
}

public struct VersionData: Decodable, Sendable, Equatable {
    public let version: String
    public let schema: Int
}

public struct StatusData: Decodable, Sendable, Equatable {
    public let topology: StatusTopology
    public let storage: StatusStorage
    public var remoteHub: RemoteHubStatus?
    public let instances: [StatusInstance]
    public let findings: [StatusFinding]

    enum CodingKeys: String, CodingKey {
        case topology
        case storage
        case remoteHub = "remote_hub"
        case instances
        case findings
    }
}

public struct RemoteHubIdentity: Decodable, Sendable, Equatable {
    public let deviceID: String?
    public let localIngestDeviceIDs: [String]?
    public let version: String

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id"
        case localIngestDeviceIDs = "local_ingest_device_ids"
        case version
    }
}

public struct RemoteHubStatus: Decodable, Sendable, Equatable {
    public let reachable: Bool
    public let supported: Bool
    public let transportPath: String?
    public let hub: RemoteHubIdentity?
    public let storageFiles: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case reachable
        case supported
        case transportPath = "transport_path"
        case hub
        case storageFiles = "storage_files"
    }
}

public struct StatusTopology: Decodable, Sendable, Equatable {
    public let mode: String
    public let stateDir: String
    public let deviceID: String?
    public let agentRoots: [StatusAgentRoot]
    public let services: [StatusService]?

    enum CodingKeys: String, CodingKey {
        case mode
        case stateDir = "state_dir"
        case deviceID = "device_id"
        case agentRoots = "agent_roots"
        case services
    }
}

public struct StatusAgentRoot: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { agent }
    public let agent: String
    public let present: Int
}

public struct StatusService: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let loaded: Bool?
    public let running: Bool?
}

public struct StatusStorage: Decodable, Sendable, Equatable {
    public let location: String
    public let files: [String: Int64]?
}

public struct StatusSources: Decodable, Sendable, Equatable {
    public let active: Double?
    public let stopped: Double?
}

public struct StatusBacklog: Decodable, Sendable, Equatable {
    public let pendingCompleteBytes: Double?
    public let uploadBytes: [String: Double]?

    enum CodingKeys: String, CodingKey {
        case pendingCompleteBytes = "pending_complete_bytes"
        case uploadBytes = "upload_bytes"
    }
}

public struct StatusInstance: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { instanceID }
    public let instanceID: String
    public let roles: [String]
    public let sampledWallMS: Int64?
    public let process: String
    public let sources: StatusSources
    public let excluded: [String: Double]?
    public let backlog: StatusBacklog?

    enum CodingKeys: String, CodingKey {
        case instanceID = "instance_id"
        case roles
        case sampledWallMS = "sampled_wall_ms"
        case process
        case sources
        case excluded
        case backlog
    }
}

public struct StatusFinding: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { "\(severity):\(code):\(evidence)" }
    public let code: String
    public let severity: String
    public let evidence: String
    public let hint: String?
}

public struct ServiceStatusData: Decodable, Sendable, Equatable {
    public let installed: Bool
    public let running: Bool
}

public struct SetupReport: Decodable, Sendable, Equatable {
    public let results: [SetupResult]

    public var isComplete: Bool {
        !results.contains(where: \.isFailure)
    }
}

public struct SetupResult: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { component }
    public let component: String
    public let status: String
    public let detail: String?

    public enum Outcome: Sendable, Equatable {
        case success
        case neutral
        case warning
        case failure
    }

    public var outcome: Outcome {
        switch status {
        case "failed", "unsupported", "blocked":
            return .failure
        case "configured", "created", "started", "restarted", "removed", "updated", "running", "ready", "saved":
            return .success
        case "unchanged", "skipped", "unmanaged", "not needed", "absent", "planned":
            return .neutral
        default:
            return .warning
        }
    }

    public var isFailure: Bool {
        outcome == .failure
    }
}

public struct SearchData: Decodable, Sendable, Equatable {
    public let matches: [SearchHit]
}

public struct SearchHit: Decodable, Sendable, Equatable, Identifiable {
    public var id: Int64 { eventID }
    public let eventID: Int64
    public let sessionID: String
    public let agent: String
    public let repo: String?
    public let timeMS: Int64?
    public let excerpt: String

    enum CodingKeys: String, CodingKey {
        case eventID = "event_id"
        case sessionID = "session_id"
        case agent
        case repo
        case timeMS = "time_ms"
        case excerpt
    }
}

public struct SessionSummary: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let deviceID: String
    public let repo: String?
    public let cwd: String?
    public let firstPrompt: String?
    public let startedAtMS: Int64?
    public let lastEventAtMS: Int64?
    public let settled: Bool?

    enum CodingKeys: String, CodingKey {
        case id
        case agent
        case deviceID = "device_id"
        case repo
        case cwd
        case firstPrompt = "first_prompt"
        case startedAtMS = "started_at_ms"
        case lastEventAtMS = "last_event_at_ms"
        case settled
    }
}

public struct SessionsData: Decodable, Sendable, Equatable {
    public let sessions: [SessionSummary]
}

public struct ShowEvent: Decodable, Sendable, Equatable, Identifiable {
    public let id: Int64
    public let kind: String
    public let role: String?
    public let toolName: String?
    public let excerpt: String

    enum CodingKeys: String, CodingKey {
        case id
        case kind
        case role
        case toolName = "tool_name"
        case excerpt
    }
}

public struct ShowData: Decodable, Sendable, Equatable {
    public let events: [ShowEvent]
}

public struct DoctorData: Decodable, Sendable, Equatable {
    public let findings: [DoctorFinding]
    public let transport: DoctorTransport?
}

public struct DoctorFinding: Decodable, Sendable, Equatable {
    public let check: String?
    public let severity: String
    public let detail: String?
    public let remedy: String?

    public init(check: String? = nil, severity: String, detail: String? = nil, remedy: String? = nil) {
        self.check = check
        self.severity = severity
        self.detail = detail
        self.remedy = remedy
    }
}

public struct DoctorTransport: Decodable, Sendable, Equatable {
    public let hubAnswers: Bool?
    public let path: String
    public let relayRegion: String?
    public let roundTripMS: Double?

    enum CodingKeys: String, CodingKey {
        case hubAnswers = "hub_answers"
        case path
        case relayRegion = "relay_region"
        case roundTripMS = "round_trip_ms"
    }
}

public struct DevicesListData: Decodable, Sendable, Equatable {
    public let devices: [PairedDevice]
}

public struct PairedDevice: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let state: String
    public let lastPath: String?

    enum CodingKeys: String, CodingKey {
        case id
        case state
        case lastPath = "last_path"
    }
}

public struct DeviceInviteData: Decodable, Sendable, Equatable {
    public let code: String
    public let expiresAt: String

    enum CodingKeys: String, CodingKey {
        case code
        case expiresAt = "expires_at"
    }
}

public struct DeviceRevokeData: Decodable, Sendable, Equatable {
    public let deviceID: String

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id"
    }
}

public struct PurgeData: Decodable, Sendable, Equatable {
    public let sessions: [PurgeSession]
    public let events: Int64
    public let bytes: Int64
}

public struct PurgeSession: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let events: Int64
}

public struct LogsData: Decodable, Sendable, Equatable {
    public let entries: [LogEntry]
}

public struct LogEntry: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { "\(timeMS):\(instanceID):\(component):\(eventCode):\(outcome ?? "")" }
    public let timeMS: Int64
    public let instanceID: String
    public let component: String
    public let level: String
    public let eventCode: String
    public let outcome: String?

    enum CodingKeys: String, CodingKey {
        case timeMS = "time_ms"
        case instanceID = "instance_id"
        case component
        case level
        case eventCode = "event_code"
        case outcome
    }
}

public struct UsageData: Decodable, Sendable, Equatable {
    public let transcript: UsageTranscript
    public let server: UsageServer
}

public struct UsageTranscript: Decodable, Sendable, Equatable {
    public let calls: Int64
}

public struct UsageServer: Decodable, Sendable, Equatable {
    public let records: Int64
}
