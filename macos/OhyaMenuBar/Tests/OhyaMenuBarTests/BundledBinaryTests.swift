import Foundation
@testable import OhyaMenuBarCore
import XCTest

final class BundledBinaryTests: XCTestCase {
    func testInstalledBinariesComeBeforeTheBundledCopy() {
        let candidates = BinaryLocator.defaultCandidates(
            home: "/Users/dev",
            bundleExecutableDir: "/Applications/Ohya.app/Contents/MacOS"
        )
        XCTAssertEqual(candidates, [
            "/Users/dev/.local/bin/ohya",
            "/opt/homebrew/bin/ohya",
            "/usr/local/bin/ohya",
            "/Applications/Ohya.app/Contents/MacOS/ohya",
        ])
    }

    func testBundledBinaryIsFoundOnlyWhenItIsExecutable() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        XCTAssertNil(BinaryLocator.bundledBinary(bundleExecutableDir: dir.path))

        let binary = dir.appendingPathComponent("ohya")
        try Data("#!/bin/sh\n".utf8).write(to: binary)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: binary.path)
        XCTAssertEqual(BinaryLocator.bundledBinary(bundleExecutableDir: dir.path), binary.path)
    }

    func testReleaseVersionOrdering() {
        XCTAssertTrue(ReleaseVersion.isNewer("v0.7.0", than: "v0.6.1"))
        XCTAssertTrue(ReleaseVersion.isNewer("v0.6.10", than: "v0.6.9"))
        XCTAssertTrue(ReleaseVersion.isNewer("v0.6.2", than: "v0.6.2-0.20261123191631-152ab92e5d12"))
        XCTAssertFalse(ReleaseVersion.isNewer("v0.6.1", than: "v0.6.1"))
        XCTAssertFalse(ReleaseVersion.isNewer("v0.6.1", than: "v0.6.2-0.20261123191631-152ab92e5d12"))
        XCTAssertFalse(ReleaseVersion.isNewer("not-a-version", than: "v0.6.1"))
        XCTAssertFalse(ReleaseVersion.isNewer("v0.7.0", than: "unknown"))
    }
}
