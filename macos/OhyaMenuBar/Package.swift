// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "OhyaMenuBar",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .library(name: "OhyaMenuBarCore", targets: ["OhyaMenuBarCore"]),
        .executable(name: "OhyaMenuBar", targets: ["OhyaMenuBar"])
    ],
    targets: [
        .target(
            name: "OhyaMenuBarCore",
            path: "Sources/OhyaMenuBarCore"
        ),
        .executableTarget(
            name: "OhyaMenuBar",
            dependencies: ["OhyaMenuBarCore"],
            path: "Sources/OhyaMenuBar",
            resources: [
                .copy("Resources/menubar-template.svg")
            ]
        ),
        .testTarget(
            name: "OhyaMenuBarTests",
            dependencies: ["OhyaMenuBarCore"],
            path: "Tests/OhyaMenuBarTests"
        )
    ]
)
