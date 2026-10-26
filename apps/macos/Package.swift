// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Albert",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "Albert", targets: ["AlbertApp"])],
    dependencies: [.package(url: "https://github.com/sparkle-project/Sparkle.git", exact: "2.10.0")],
    targets: [
        .target(name: "AlbertCore"),
        .target(name: "AlbertUI", dependencies: ["AlbertCore", .product(name: "Sparkle", package: "Sparkle")]),
        .executableTarget(name: "AlbertApp", dependencies: ["AlbertUI"]),
        .testTarget(name: "AlbertCoreTests", dependencies: ["AlbertCore"]),
        .testTarget(name: "AlbertUITests", dependencies: ["AlbertUI", "AlbertCore"]),
    ]
)
