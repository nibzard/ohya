import Foundation
@testable import OhyaMenuBarCore
import XCTest

final class FirstRunSetupTests: XCTestCase {
    private let decoder = JSONDecoder()

    func testDecodeServiceStatusReportsMissingInstall() throws {
        let json = """
        {"schema_version":1,"command":"ohya service status","data":{"service":"service","action":"status","installed":false,"edited":false,"loaded":false,"running":false,"changed":false,"detail":"not configured; run ohya setup"},"error":null}
        """
        let envelope = try decoder.decode(JSONEnvelope<ServiceStatusData>.self, from: Data(json.utf8))
        XCTAssertEqual(envelope.data?.installed, false)
        XCTAssertEqual(envelope.data?.running, false)
    }

    func testDecodeCompleteAndIncompleteSetupReports() throws {
        let complete = """
        {"schema_version":1,"command":"ohya setup","data":{"results":[{"component":"service","status":"running","detail":"History imports in the background; run ohya doctor to check sources.","changed":true},{"component":"claude","status":"configured","detail":"Open a new agent session to load ohya.","changed":true}]},"error":null}
        """
        let report = try XCTUnwrap(decoder.decode(JSONEnvelope<SetupReport>.self, from: Data(complete.utf8)).data)
        XCTAssertTrue(report.isComplete)
        XCTAssertEqual(report.results.map(\.component), ["service", "claude"])

        let incomplete = """
        {"schema_version":1,"command":"ohya setup","data":{"results":[{"component":"service","status":"running","detail":"","changed":false},{"component":"codex","status":"failed","detail":"/Users/dev/.codex/skills/ohya/SKILL.md is not owned by setup","changed":false},{"component":"pi","status":"blocked","detail":"pi is older than the supported release","changed":false}]},"error":null}
        """
        let partial = try XCTUnwrap(decoder.decode(JSONEnvelope<SetupReport>.self, from: Data(incomplete.utf8)).data)
        XCTAssertFalse(partial.isComplete)
        XCTAssertEqual(partial.results.filter(\.isFailure).map(\.component), ["codex", "pi"])
    }

    func testSetupResultOutcomesMatchTheCLIMarks() throws {
        let json = """
        {"results":[{"component":"path","status":"configured"},{"component":"claude","status":"unchanged"},{"component":"path-command","status":"warning"},{"component":"pi","status":"blocked"},{"component":"codex","status":"failed"}]}
        """
        let report = try decoder.decode(SetupReport.self, from: Data(json.utf8))
        XCTAssertEqual(report.results.map(\.outcome), [.success, .neutral, .warning, .failure, .failure])
        XCTAssertFalse(report.isComplete)
    }

    func testSetupCommandInstallsABundledBinaryAndSetsUpAnInstalledOne() {
        let bundled = SetupCommand.arguments(
            binaryPath: "/Applications/Ohya.app/Contents/MacOS/ohya",
            bundleExecutableDir: "/Applications/Ohya.app/Contents/MacOS",
            joinCodeVariable: nil
        )
        XCTAssertEqual(bundled, ["install", "--json"])

        let installed = SetupCommand.arguments(
            binaryPath: "/Users/dev/.local/bin/ohya",
            bundleExecutableDir: "/Applications/Ohya.app/Contents/MacOS",
            joinCodeVariable: SetupCommand.joinCodeVariable
        )
        XCTAssertEqual(installed, ["setup", "--json", "--join-code-env", "OHYA_JOIN_CODE"])
    }

    func testRunnerPassesTheJoinCodeOnlyThroughTheEnvironmentAndKeepsAnIncompleteReport() async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let script = dir.appendingPathComponent("ohya")
        let body = """
        #!/bin/sh
        printf '%s\\n' "$@" > "\(dir.path)/args"
        printf '%s' "$OHYA_JOIN_CODE" > "\(dir.path)/code"
        echo '{"schema_version":1,"command":"ohya setup","data":{"results":[{"component":"codex","status":"failed","detail":"edited","changed":false}]},"error":null}'
        exit 1
        """
        try body.write(to: script, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

        let runner = OhyaRunner()
        let response: CLIResponse<SetupReport> = try await runner.runJSON(
            binaryPath: script.path,
            arguments: ["setup", "--json", "--join-code-env", SetupCommand.joinCodeVariable],
            timeout: 10,
            environment: [SetupCommand.joinCodeVariable: "ohya1.secret-code"],
            acceptReportOnFailure: true
        )
        XCTAssertEqual(response.exitCode, 1)
        XCTAssertFalse(response.data.isComplete)

        let args = try String(contentsOf: dir.appendingPathComponent("args"), encoding: .utf8)
        XCTAssertFalse(args.contains("ohya1.secret-code"))
        let code = try String(contentsOf: dir.appendingPathComponent("code"), encoding: .utf8)
        XCTAssertEqual(code, "ohya1.secret-code")
    }
}
