import Foundation
@testable import OhyaMenuBarCore
import XCTest

final class OhyaMenuBarCoreTests: XCTestCase {
    private let decoder = JSONDecoder()

    func testDecodeStatusEnvelope() throws {
        let json = """
        {
          "schema_version": 1,
          "command": "ohya status",
          "data": {
            "format_version": 1,
            "generated_wall_ms": 1763740000000,
            "generated_utc": "2026-11-21T16:00:00Z",
            "topology": {
              "mode": "local",
              "state_dir": "/Users/dev/.ohya",
              "agent_roots": [
                {"agent": "claude-code", "roots": 1, "present": 1},
                {"agent": "codex", "roots": 2, "present": 2},
                {"agent": "pi", "roots": 1, "present": 0}
              ],
              "services": [
                {"name": "service", "loaded": true, "running": true},
                {"name": "hub-service", "loaded": true, "running": true}
              ]
            },
            "storage": {
              "location": "local",
              "path": "/Users/dev/.ohya/hub.db",
              "files": {"db": 1048576, "wal": 2048}
            },
            "remote_hub": {
              "reachable": true,
              "supported": true,
              "transport_path": "direct",
              "hub": {"version": "v0.3.0", "revision": "abc1234", "dirty": "false", "go": "go1.27.3", "os": "linux", "arch": "amd64", "schema": 8},
              "uptime_ms": 60000,
              "storage_files": {"db": 2097152, "wal": 4096}
            },
            "instances": [
              {
                "instance_id": "abc12345def67890",
                "pid": 4200,
                "roles": ["ingest"],
                "uptime_ms": 60000,
                "sampled_wall_ms": 1763739995000,
                "sample_age_ms": 5000,
                "fresh": true,
                "process": "running",
                "sources": {"active": 12, "stopped": 0, "blocked": 0, "missing": 0, "unknown": 0},
                "excluded": {"codex": 2},
                "backlog": {"pending_complete_bytes": 0, "partial_tail_bytes": 0}
              }
            ],
            "findings": [
              {"code": "observation_current", "severity": "ok", "evidence": "fresh"}
            ],
            "coverage": {
              "present": ["ingest"],
              "truncated_discovery": false
            }
          },
          "error": null
        }
        """

        let env = try decoder.decode(JSONEnvelope<StatusData>.self, from: Data(json.utf8))
        XCTAssertEqual(env.schemaVersion, 1)
        XCTAssertNil(env.error)
        let data = try XCTUnwrap(env.data)
        XCTAssertEqual(data.topology.agentRoots.count, 3)

        let summary = StatusEvaluator.evaluate(status: data, error: nil)
        XCTAssertEqual(summary.iconState, .caughtUp)
        XCTAssertEqual(summary.topologyLabel, "Hub")
        XCTAssertTrue(summary.isHub)
        XCTAssertEqual(summary.activeSources, 12)
        XCTAssertEqual(summary.stoppedSources, 0)

        XCTAssertFalse(summary.isPaused)

        let stoppedJSON = json.replacingOccurrences(
            of: #"{"name": "service", "loaded": true, "running": true}"#,
            with: #"{"name": "service", "loaded": false, "running": false}"#
        )
        XCTAssertNotEqual(stoppedJSON, json)
        let stoppedData = try XCTUnwrap(decoder.decode(JSONEnvelope<StatusData>.self, from: Data(stoppedJSON.utf8)).data)
        XCTAssertTrue(StatusEvaluator.evaluate(status: stoppedData, error: nil).isPaused)

        let failed = StatusEvaluator.evaluate(status: data, error: .timedOut(command: ["status", "--json"], seconds: 10))
        XCTAssertEqual(failed.iconState, .unavailable)
        XCTAssertEqual(failed.topologyLabel, "Hub")
        XCTAssertEqual(failed.activeSources, 12)
    }

    func testDecodeServiceSearchShowAndSessionsEnvelopes() throws {
        let serviceJSON = """
        {"schema_version":1,"command":"ohya service stop","data":{"service":"service","action":"stop","installed":true,"edited":false,"loaded":false,"running":false,"changed":true,"path":"/Users/dev/Library/LaunchAgents/com.nibzard.ohya.plist"},"error":null}
        """
        let serviceEnv = try decoder.decode(JSONEnvelope<ServiceStatusData>.self, from: Data(serviceJSON.utf8))
        XCTAssertEqual(serviceEnv.data?.running, false)

        let sessionsJSON = """
        {"schema_version":1,"command":"ohya sessions","data":{"sessions":[{"id":"claude-code:dev1:s1","agent":"claude-code","device_id":"dev1","native_id":"s1","repo":"github.com/nibzard/ohya","cwd":"/Users/dev/ohya","first_prompt":"Add menubar","event_count":4,"settled":false}],"truncated":false},"error":null}
        """
        let sessionsEnv = try decoder.decode(JSONEnvelope<SessionsData>.self, from: Data(sessionsJSON.utf8))
        let session = try XCTUnwrap(sessionsEnv.data?.sessions.first)
        XCTAssertEqual(session.settled, false)

        let prompt = HandoffPromptBuilder.build(for: session)
        XCTAssertTrue(prompt.contains("claude-code:dev1:s1"))
        XCTAssertTrue(prompt.contains("nibzard/ohya"))
        XCTAssertTrue(prompt.contains("events directly"))

        let searchJSON = """
        {"schema_version":1,"command":"ohya search","data":{"query":"refresh","matches":[{"event_id":7,"session_id":"claude-code:dev1:s1","agent":"claude-code","repo":"github.com/nibzard/ohya","time":"2026-11-21T16:00:00Z","time_ms":1763740000000,"excerpt":"token refresh"}],"truncated":false,"redactions":1},"error":null}
        """
        let searchEnv = try decoder.decode(JSONEnvelope<SearchData>.self, from: Data(searchJSON.utf8))
        XCTAssertEqual(searchEnv.data?.matches.count, 1)
        XCTAssertEqual(searchEnv.data?.matches.first?.eventID, 7)

        let showJSON = """
        {"schema_version":1,"command":"ohya show","data":{"session":{"id":"claude-code:dev1:s1","agent":"claude-code","device_id":"dev1","native_id":"s1","event_count":2,"settled":true},"events":[{"id":1,"kind":"message","role":"user","excerpt":"hello"}],"truncated":false},"error":null}
        """
        let showEnv = try decoder.decode(JSONEnvelope<ShowData>.self, from: Data(showJSON.utf8))
        XCTAssertEqual(showEnv.data?.events.count, 1)
        XCTAssertEqual(showEnv.data?.events.first?.excerpt, "hello")
    }

    func testSessionOriginNamesThisMacTheHubAndOtherDevices() throws {
        let local = "a4ca22cc3f546eeffd469f74abade288"
        let hub = "05b0ff2c233cb6a22d24cb1722f9204f"
        let third = "77e1d0c4aa0b4c1e9f0d2b3c4d5e6f70"
        let joinedJSON = """
        {
          "topology": {"mode": "remote", "state_dir": "/Users/dev/.ohya/join", "device_id": "\(local)", "agent_roots": []},
          "storage": {"location": "remote-hub"},
          "remote_hub": {"reachable": true, "supported": true, "hub": {"device_id": "\(hub)", "version": "v0.3.0"}},
          "instances": [],
          "findings": []
        }
        """
        let joined = try decoder.decode(StatusData.self, from: Data(joinedJSON.utf8))
        XCTAssertEqual(joined.topology.deviceID, local)
        XCTAssertEqual(joined.remoteHub?.hub?.deviceID, hub)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: local, status: joined), .thisMac)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: hub, status: joined), .hub)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: third, status: joined), .device("77e1d0c4"))

        let splitStateJSON = joinedJSON.replacingOccurrences(
            of: #""device_id": "\#(hub)", "#,
            with: #""device_id": "\#(hub)", "local_ingest_device_ids": ["\#(third)"], "#
        )
        let splitState = try decoder.decode(StatusData.self, from: Data(splitStateJSON.utf8))
        XCTAssertEqual(SessionOrigin.resolve(deviceID: third, status: splitState), .hub)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: hub, status: splitState), .device("05b0ff2c"))
        XCTAssertEqual(SessionOrigin.resolve(deviceID: local, status: splitState), .thisMac)

        let hubJSON = joinedJSON
            .replacingOccurrences(of: #""mode": "remote""#, with: #""mode": "local""#)
            .replacingOccurrences(of: #""device_id": "\#(local)""#, with: #""device_id": "\#(hub)""#)
        let onHub = try decoder.decode(StatusData.self, from: Data(hubJSON.utf8))
        XCTAssertEqual(SessionOrigin.resolve(deviceID: hub, status: onHub), .thisMac)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: local, status: onHub), .device("a4ca22cc"))

        let olderHubJSON = joinedJSON.replacingOccurrences(of: #""device_id": "\#(hub)", "#, with: "")
        let olderHub = try decoder.decode(StatusData.self, from: Data(olderHubJSON.utf8))
        XCTAssertNil(olderHub.remoteHub?.hub?.deviceID)
        XCTAssertEqual(SessionOrigin.resolve(deviceID: hub, status: olderHub), .device("05b0ff2c"))

        XCTAssertEqual(SessionOrigin.resolve(deviceID: local, status: nil), .device("a4ca22cc"))
        XCTAssertEqual(SessionOrigin.thisMac.label, "This Mac")
        XCTAssertEqual(SessionOrigin.hub.label, "Hub")
        XCTAssertEqual(SessionOrigin.device("77e1d0c4").label, "77e1d0c4")
    }

    func testFTSQueryFormatterAutoQuotesPunctuationPreservingOperators() {
        XCTAssertEqual(FTSQueryFormatter.format("auth-refresh"), "\"auth-refresh\"")
        XCTAssertEqual(FTSQueryFormatter.format("auth AND refresh"), "auth AND refresh")
        XCTAssertEqual(FTSQueryFormatter.format("feature/auth-rework AND panic"), "\"feature/auth-rework\" AND panic")
        XCTAssertEqual(FTSQueryFormatter.format("\"exact phrase\""), "\"exact phrase\"")
        XCTAssertEqual(FTSQueryFormatter.format("don't fail"), "\"don't\" fail")
        XCTAssertEqual(FTSQueryFormatter.format("   "), "")
    }

    func testTransitionAlertsFireOnlyOnDegradation() {
        let healthy = StatusSummary(
            iconState: .caughtUp,
            topologyLabel: "Local",
            isHub: false,
            isRemote: false,
            totalBacklogBytes: 0,
            stoppedSources: 0,
            activeSources: 5,
            warnings: [],
            isPaused: false
        )
        let stopped = StatusSummary(
            iconState: .degraded,
            topologyLabel: "Local",
            isHub: false,
            isRemote: false,
            totalBacklogBytes: 0,
            stoppedSources: 1,
            activeSources: 4,
            warnings: [StatusFinding(code: "sources_stopped", severity: "warn", evidence: "1 stopped", hint: nil)],
            isPaused: false
        )
        let paused = StatusSummary(
            iconState: .paused,
            topologyLabel: "Local",
            isHub: false,
            isRemote: false,
            totalBacklogBytes: 0,
            stoppedSources: 0,
            activeSources: 0,
            warnings: [StatusFinding(code: "no_instances", severity: "warn", evidence: "stopped", hint: nil)],
            isPaused: true
        )

        XCTAssertNil(StatusEvaluator.transitionAlert(previous: nil, current: stopped))
        XCTAssertEqual(StatusEvaluator.transitionAlert(previous: healthy, current: stopped)?.identifier, "ohya.source.stopped")
        XCTAssertNil(StatusEvaluator.transitionAlert(previous: stopped, current: stopped))
        XCTAssertNil(StatusEvaluator.transitionAlert(previous: healthy, current: paused))
    }

    func testDisplayFormatting() {
        XCTAssertEqual(DisplayFormat.bytes(512), "512 B")
        XCTAssertEqual(DisplayFormat.bytes(1536), "1.5 KB")
        XCTAssertEqual(DisplayFormat.bytes(1048576 * 12.4), "12.4 MB")
        XCTAssertEqual(DisplayFormat.shortID("0123456789abcdef"), "01234567")
        XCTAssertEqual(DisplayFormat.agentDisplayName("claude-code"), "Claude Code")
    }

    func testRunnerErrorCopyableTextIncludesContext() {
        let notFound = OhyaRunnerError.binaryNotFound(["/Users/dev/.local/bin/ohya", "/opt/homebrew/bin/ohya"])
        XCTAssertTrue(notFound.copyableText.contains("/Users/dev/.local/bin/ohya"))
        XCTAssertTrue(notFound.copyableText.contains("Install ohya"))

        let failed = OhyaRunnerError.commandFailed(
            code: "db_locked",
            message: "database is locked",
            hint: "Check running ohya processes.",
            exitCode: 1
        )
        XCTAssertEqual(failed.copyableText, "[db_locked, exit 1] database is locked\nCheck running ohya processes.")

        let timedOut = OhyaRunnerError.timedOut(command: ["status", "--json"], seconds: 10)
        XCTAssertEqual(timedOut.copyableText, "ohya status --json timed out after 10s.")
    }

    func testCopyFormatterFormatsFindingsLogsSetupAndStatus() throws {
        let finding = StatusFinding(
            code: "sources_stopped",
            severity: "warn",
            evidence: "1 source stopped",
            hint: "Run ohya doctor."
        )
        XCTAssertEqual(CopyFormatter.finding(finding), "[warn] sources_stopped: 1 source stopped\nRun ohya doctor.")

        let setupJSON = """
        {"results":[{"component":"service","status":"running","detail":""},{"component":"codex","status":"failed","detail":"skill not owned"}]}
        """
        let setupReport = try decoder.decode(SetupReport.self, from: Data(setupJSON.utf8))
        XCTAssertEqual(
            CopyFormatter.setupReport(setupReport),
            "Setup incomplete\n- service: running\n- codex: failed — skill not owned"
        )

        let logEntry = LogEntry(
            timeMS: 1763740000000,
            instanceID: "abc12345def67890",
            component: "ingest",
            level: "error",
            eventCode: "source_stopped",
            outcome: "malformed_line"
        )
        XCTAssertEqual(
            CopyFormatter.logs([logEntry]),
            "2025-11-21T15:46:40Z ERROR ingest source_stopped (malformed_line) [abc12345]"
        )

        let doctorJSON = """
        {"schema_version":1,"command":"ohya doctor","data":{"findings":[{"check":"db_schema","severity":"ok","detail":"schema 8","remedy":""},{"check":"stopped_sources","severity":"error","detail":"1 stopped source","remedy":"ohya doctor --max-record-bytes"}]},"error":null}
        """
        let doctor = try XCTUnwrap(decoder.decode(JSONEnvelope<DoctorData>.self, from: Data(doctorJSON.utf8)).data)
        XCTAssertEqual(doctor.findings.count, 2)
        XCTAssertEqual(doctor.findings[1].check, "stopped_sources")
        XCTAssertEqual(doctor.findings[1].remedy, "ohya doctor --max-record-bytes")

        let summary = StatusSummary(
            iconState: .degraded,
            topologyLabel: "Local",
            isHub: false,
            isRemote: false,
            totalBacklogBytes: 2048,
            stoppedSources: 1,
            activeSources: 4,
            warnings: [finding],
            isPaused: false
        )
        let text = CopyFormatter.statusSummary(
            version: VersionData(version: "v0.6.2", schema: 8),
            binaryPath: "/Users/dev/.local/bin/ohya",
            setupNeeded: false,
            summary: summary,
            status: nil,
            doctor: doctor,
            lastError: nil,
            updateError: nil,
            actionError: "service stop failed"
        )
        XCTAssertTrue(text.contains("ohya v0.6.2 (schema 8)"))
        XCTAssertTrue(text.contains("Status: Local · Attention"))
        XCTAssertTrue(text.contains("Binary: /Users/dev/.local/bin/ohya"))
        XCTAssertTrue(text.contains("Action error: service stop failed"))
        XCTAssertTrue(text.contains("[warn] sources_stopped: 1 source stopped"))
        XCTAssertTrue(text.contains("[error] stopped_sources: 1 stopped source"))
        XCTAssertTrue(text.contains("Remedy: ohya doctor --max-record-bytes"))
        XCTAssertFalse(text.contains("db_schema"))
    }
}
