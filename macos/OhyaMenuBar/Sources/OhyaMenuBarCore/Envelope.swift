import Foundation

public struct JSONEnvelope<T: Decodable & Sendable>: Decodable, Sendable {
    public let schemaVersion: Int
    public let command: String
    public let data: T?
    public let error: CLIErrorPayload?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case command
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
    public let revision: String
    public let dirty: String
    public let go: String
    public let schema: Int
    public let parserClaude: Int
    public let parserCodex: Int
    public let parserPi: Int

    enum CodingKeys: String, CodingKey {
        case version
        case revision
        case dirty
        case go
        case schema
        case parserClaude = "parser.claude"
        case parserCodex = "parser.codex"
        case parserPi = "parser.pi"
    }
}

public struct StatusData: Decodable, Sendable, Equatable {
    public let formatVersion: Int
    public let generatedWallMS: Int64
    public let generatedUTC: String
    public let topology: StatusTopology
    public let storage: StatusStorage
    public let instances: [StatusInstance]
    public let findings: [StatusFinding]
    public let coverage: StatusCoverage

    enum CodingKeys: String, CodingKey {
        case formatVersion = "format_version"
        case generatedWallMS = "generated_wall_ms"
        case generatedUTC = "generated_utc"
        case topology
        case storage
        case instances
        case findings
        case coverage
    }
}

public struct StatusTopology: Decodable, Sendable, Equatable {
    public let mode: String
    public let stateDir: String
    public let ambiguity: [String]?
    public let agentRoots: [StatusAgentRoot]
    public let services: [StatusService]?

    enum CodingKeys: String, CodingKey {
        case mode
        case stateDir = "state_dir"
        case ambiguity
        case agentRoots = "agent_roots"
        case services
    }
}

public struct StatusAgentRoot: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { agent }
    public let agent: String
    public let roots: Int
    public let present: Int
}

public struct StatusService: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { name }
    public let name: String
    public let loaded: Bool?
    public let running: Bool?
    public let detail: String?
}

public struct StatusStorage: Decodable, Sendable, Equatable {
    public let location: String
    public let path: String?
    public let files: [String: Int64]?
    public let observedWallMS: Int64?
    public let source: String?

    enum CodingKeys: String, CodingKey {
        case location
        case path
        case files
        case observedWallMS = "observed_wall_ms"
        case source
    }
}

public struct StatusSources: Decodable, Sendable, Equatable {
    public let active: Double?
    public let stopped: Double?
    public let blocked: Double?
    public let missing: Double?
    public let unknown: Double?
}

public struct StatusBacklog: Decodable, Sendable, Equatable {
    public let pendingCompleteBytes: Double?
    public let partialTailBytes: Double?
    public let uploadBytes: [String: Double]?

    enum CodingKeys: String, CodingKey {
        case pendingCompleteBytes = "pending_complete_bytes"
        case partialTailBytes = "partial_tail_bytes"
        case uploadBytes = "upload_bytes"
    }
}

public struct ResourceReading: Decodable, Sendable, Equatable {
    public let value: Double?
    public let reason: String?
}

public struct StatusInstance: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { instanceID }
    public let instanceID: String
    public let pid: Int
    public let roles: [String]
    public let uptimeMS: Int64?
    public let sampledWallMS: Int64?
    public let sampleAgeMS: Int64?
    public let fresh: Bool
    public let process: String
    public let processDetail: String?
    public let sources: StatusSources
    public let excluded: [String: Double]?
    public let backlog: StatusBacklog?
    public let lastScanMS: Double?
    public let resources: [String: ResourceReading]?
    public let decodeError: String?

    enum CodingKeys: String, CodingKey {
        case instanceID = "instance_id"
        case pid
        case roles
        case uptimeMS = "uptime_ms"
        case sampledWallMS = "sampled_wall_ms"
        case sampleAgeMS = "sample_age_ms"
        case fresh
        case process
        case processDetail = "process_detail"
        case sources
        case excluded
        case backlog
        case lastScanMS = "last_scan_ms"
        case resources
        case decodeError = "decode_error"
    }
}

public struct StatusFinding: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { "\(severity):\(code):\(evidence)" }
    public let code: String
    public let severity: String
    public let evidence: String
    public let hint: String?
}

public struct StatusCoverage: Decodable, Sendable, Equatable {
    public let present: [String]?
    public let missing: [String]?
    public let stale: [String]?
    public let unsupported: [String]?
    public let truncatedDiscovery: Bool
    public let notes: [String]?

    enum CodingKeys: String, CodingKey {
        case present
        case missing
        case stale
        case unsupported
        case truncatedDiscovery = "truncated_discovery"
        case notes
    }
}

public struct ServiceStatusData: Decodable, Sendable, Equatable {
    public let service: String
    public let action: String
    public let installed: Bool
    public let edited: Bool
    public let loaded: Bool
    public let running: Bool
    public let changed: Bool
    public let path: String?
    public let detail: String?
}

public struct SearchData: Decodable, Sendable, Equatable {
    public let query: String
    public let matches: [SearchHit]
    public let truncated: Bool
    public let truncationReason: String?
    public let redactions: Int?

    enum CodingKeys: String, CodingKey {
        case query
        case matches
        case truncated
        case truncationReason = "truncation_reason"
        case redactions
    }
}

public struct SearchHit: Decodable, Sendable, Equatable, Identifiable {
    public var id: Int64 { eventID }
    public let eventID: Int64
    public let sessionID: String
    public let agent: String
    public let repo: String?
    public let time: String?
    public let timeMS: Int64?
    public let excerpt: String

    enum CodingKeys: String, CodingKey {
        case eventID = "event_id"
        case sessionID = "session_id"
        case agent
        case repo
        case time
        case timeMS = "time_ms"
        case excerpt
    }
}

public struct SessionSummary: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let deviceID: String
    public let nativeID: String
    public let repo: String?
    public let cwd: String?
    public let branch: String?
    public let model: String?
    public let firstPrompt: String?
    public let startedAt: String?
    public let startedAtMS: Int64?
    public let lastEventAt: String?
    public let lastEventAtMS: Int64?
    public let eventCount: Int64
    public let settled: Bool?

    enum CodingKeys: String, CodingKey {
        case id
        case agent
        case deviceID = "device_id"
        case nativeID = "native_id"
        case repo
        case cwd
        case branch
        case model
        case firstPrompt = "first_prompt"
        case startedAt = "started_at"
        case startedAtMS = "started_at_ms"
        case lastEventAt = "last_event_at"
        case lastEventAtMS = "last_event_at_ms"
        case eventCount = "event_count"
        case settled
    }
}

public struct SessionsData: Decodable, Sendable, Equatable {
    public let sessions: [SessionSummary]
    public let truncated: Bool
    public let truncationReason: String?
    public let redactions: Int?

    enum CodingKeys: String, CodingKey {
        case sessions
        case truncated
        case truncationReason = "truncation_reason"
        case redactions
    }
}

public struct ShowEvent: Decodable, Sendable, Equatable, Identifiable {
    public let id: Int64
    public let time: String?
    public let timeMS: Int64?
    public let kind: String
    public let role: String?
    public let toolName: String?
    public let excerpt: String

    enum CodingKeys: String, CodingKey {
        case id
        case time
        case timeMS = "time_ms"
        case kind
        case role
        case toolName = "tool_name"
        case excerpt
    }
}

public struct ShowData: Decodable, Sendable, Equatable {
    public let session: SessionSummary
    public let events: [ShowEvent]
    public let truncated: Bool
    public let truncationReason: String?
    public let redactions: Int?

    enum CodingKeys: String, CodingKey {
        case session
        case events
        case truncated
        case truncationReason = "truncation_reason"
        case redactions
    }
}

public struct DoctorData: Decodable, Sendable, Equatable {
    public let findings: [DoctorFinding]
    public let transport: DoctorTransport?
}

public struct DoctorFinding: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { "\(check):\(severity):\(detail)" }
    public let check: String
    public let severity: String
    public let detail: String
    public let remedy: String
}

public struct DoctorTransport: Decodable, Sendable, Equatable {
    public let hubAnswers: Bool?
    public let path: String
    public let relayRegion: String?
    public let roundTripMS: Double?
    public let protocolVersion: Int
    public let protocolCompatible: Bool?

    enum CodingKeys: String, CodingKey {
        case hubAnswers = "hub_answers"
        case path
        case relayRegion = "relay_region"
        case roundTripMS = "round_trip_ms"
        case protocolVersion = "protocol_version"
        case protocolCompatible = "protocol_compatible"
    }
}

public struct DevicesListData: Decodable, Sendable, Equatable {
    public let devices: [PairedDevice]
    public let invitations: [DeviceInvitation]
}

public struct PairedDevice: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let state: String
    public let pairedAt: String
    public let revokedAt: String?
    public let lastPath: String?
    public let lastSyncAt: String?

    enum CodingKeys: String, CodingKey {
        case id
        case state
        case pairedAt = "paired_at"
        case revokedAt = "revoked_at"
        case lastPath = "last_path"
        case lastSyncAt = "last_sync_at"
    }
}

public struct DeviceInvitation: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let state: String
    public let usedBy: String?
    public let createdAt: String
    public let expiresAt: String
    public let expired: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case state
        case usedBy = "used_by"
        case createdAt = "created_at"
        case expiresAt = "expires_at"
        case expired
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
    public let dryRun: Bool
    public let scope: String
    public let sessions: [PurgeSession]
    public let sources: [PurgeSource]
    public let events: Int64
    public let bytes: Int64

    enum CodingKeys: String, CodingKey {
        case dryRun = "dry_run"
        case scope
        case sessions
        case sources
        case events
        case bytes
    }
}

public struct PurgeSession: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let deviceID: String
    public let nativeID: String
    public let events: Int64

    enum CodingKeys: String, CodingKey {
        case id
        case agent
        case deviceID = "device_id"
        case nativeID = "native_id"
        case events
    }
}

public struct PurgeSource: Decodable, Sendable, Equatable, Identifiable {
    public let id: String
    public let agent: String
    public let deviceID: String
    public let path: String
    public let cursorBytes: Int64
    public let events: Int64
    public let nativeID: String?
    public let purgedAt: String?

    enum CodingKeys: String, CodingKey {
        case id
        case agent
        case deviceID = "device_id"
        case path
        case cursorBytes = "cursor_bytes"
        case events
        case nativeID = "native_id"
        case purgedAt = "purged_at"
    }
}

public struct LogsData: Decodable, Sendable, Equatable {
    public let formatVersion: Int
    public let generatedUTC: String
    public let entries: [LogEntry]
    public let truncated: Bool
    public let truncationReason: String?

    enum CodingKeys: String, CodingKey {
        case formatVersion = "format_version"
        case generatedUTC = "generated_utc"
        case entries
        case truncated
        case truncationReason = "truncation_reason"
    }
}

public struct LogEntry: Decodable, Sendable, Equatable, Identifiable {
    public var id: String { "\(timeMS):\(instanceID):\(component):\(eventCode):\(outcome ?? "")" }
    public let formatVersion: Int
    public let timeMS: Int64
    public let instanceID: String
    public let component: String
    public let level: String
    public let eventCode: String
    public let outcome: String?
    public let durationMS: Double?
    public let counts: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case formatVersion = "format_version"
        case timeMS = "time_ms"
        case instanceID = "instance_id"
        case component
        case level
        case eventCode = "event_code"
        case outcome
        case durationMS = "duration_ms"
        case counts
    }
}

public struct UsageData: Decodable, Sendable, Equatable {
    public let schemaVersion: Int
    public let sinceMS: Int64
    public let untilMS: Int64
    public let partial: Bool
    public let transcript: UsageTranscript
    public let server: UsageServer

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case sinceMS = "since_ms"
        case untilMS = "until_ms"
        case partial
        case transcript
        case server
    }
}

public struct UsageTranscript: Decodable, Sendable, Equatable {
    public let calls: Int64
    public let sessionsWithCalls: Int64

    enum CodingKeys: String, CodingKey {
        case calls
        case sessionsWithCalls = "sessions_with_calls"
    }
}

public struct UsageServer: Decodable, Sendable, Equatable {
    public let found: Bool
    public let records: Int64
    public let byToolClientWeek: [UsageServerWeek]?

    enum CodingKeys: String, CodingKey {
        case found
        case records
        case byToolClientWeek = "by_tool_client_week"
    }
}

public struct UsageServerWeek: Decodable, Sendable, Equatable {
    public let tool: String
    public let client: String
    public let path: String
    public let week: String
    public let calls: Int64
}
