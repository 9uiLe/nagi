// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "NagiFixture",
    products: [
        .library(name: "NagiFixture", targets: ["NagiFixture"])
    ],
    targets: [
        .target(name: "NagiFixture"),
        .testTarget(name: "NagiFixtureTests", dependencies: ["NagiFixture"])
    ]
)
