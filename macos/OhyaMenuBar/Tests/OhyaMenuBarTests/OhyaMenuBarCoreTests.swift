import Foundation
import OhyaMenuBarCore
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

        let summary = StatusEvaluator.evaluate(status: data, service: nil, error: nil)
        XCTAssertEqual(summary.iconState, .caughtUp)
        XCTAssertEqual(summary.topologyLabel, "Hub")
        XCTAssertTrue(summary.isHub)
        XCTAssertEqual(summary.activeSources, 12)
        XCTAssertEqual(summary.stoppedSources, 0)
    }

    func testDecodeServiceSearchShowAndSessionsEnvelopes() throws {
        let serviceJSON = """
        {"schema_version":1,"command":"ohya service stop","data":{"service":"service","action":"stop","installed":true,"edited":false,"loaded":false,"running":false,"changed":true,"path":"/Users/dev/Library/LaunchAgents/com.nibzard.ohya.plist"},"error":null}
        """
        let serviceEnv = try decoder.decode(JSONEnvelope<ServiceStatusData>.self, from: Data(serviceJSON.utf8))
        XCTAssertEqual(serviceEnv.data?.changed, true)
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
        XCTAssertEqual(searchEnv.data?.redactions, 1)

        let showJSON = """
        {"schema_version":1,"command":"ohya show","data":{"session":{"id":"claude-code:dev1:s1","agent":"claude-code","device_id":"dev1","native_id":"s1","event_count":2,"settled":true},"events":[{"id":1,"kind":"message","role":"user","excerpt":"hello"}],"truncated":false},"error":null}
        """
        let showEnv = try decoder.decode(JSONEnvelope<ShowData>.self, from: Data(showJSON.utf8))
        XCTAssertEqual(showEnv.data?.events.count, 1)
        XCTAssertEqual(showEnv.data?.session.settled, true)
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
}
