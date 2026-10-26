import AppKit
import Foundation

// Run through scripts/generate-icons.sh. Never requires a running queue or app.
@main
struct GenerateIcons {
    @MainActor
    static func main() throws {
        guard CommandLine.arguments.count == 3 else {
            throw NSError(domain: "GenerateIcons", code: 1, userInfo: [NSLocalizedDescriptionKey: "Expected repository and iconset directories"])
        }
        let root = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
        let iconset = URL(fileURLWithPath: CommandLine.arguments[2], isDirectory: true)
        for points in [16, 32, 128, 256, 512] {
            for scale in [1, 2] {
                let name = "icon_\(points)x\(points)\(scale == 2 ? "@2x" : "").png"
                try render(size: points * scale, desktop: true, rounded: true).write(to: iconset.appendingPathComponent(name))
            }
        }
        for (name, size, rounded) in [("app-icon.png", 256, true), ("favicon.png", 64, true), ("apple-touch-icon.png", 180, false)] {
            try render(size: size, desktop: false, rounded: rounded).write(to: root.appendingPathComponent("remote/static/\(name)"))
        }
    }

    @MainActor
    static func render(size: Int, desktop: Bool, rounded: Bool) throws -> Data {
        guard let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
                                            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                                            isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
              let context = NSGraphicsContext(bitmapImageRep: bitmap),
              let symbol = NSImage(systemSymbolName: QueueBrand.symbolName, accessibilityDescription: nil)?
                .withSymbolConfiguration(.init(pointSize: 640, weight: .regular))?
                .withSymbolConfiguration(.init(paletteColors: [.white])) else {
            throw NSError(domain: "GenerateIcons", code: 2, userInfo: [NSLocalizedDescriptionKey: "Could not render the queue symbol"])
        }
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = context
        defer { NSGraphicsContext.restoreGraphicsState() }
        context.cgContext.scaleBy(x: CGFloat(size) / 1024, y: CGFloat(size) / 1024)
        context.cgContext.clear(CGRect(x: 0, y: 0, width: 1024, height: 1024))
        context.imageInterpolation = .high
        let inset: CGFloat = desktop ? 80 : 0
        let tile = NSRect(x: inset, y: inset, width: 1024 - 2 * inset, height: 1024 - 2 * inset)
        NSColor(srgbRed: 0, green: 0.4, blue: 0.8, alpha: 1).setFill()
        NSBezierPath(roundedRect: tile, xRadius: rounded ? tile.width * 0.22 : 0,
                     yRadius: rounded ? tile.height * 0.22 : 0).fill()
        let width = tile.width * 0.65
        let height = width * symbol.size.height / symbol.size.width
        symbol.draw(in: NSRect(x: tile.midX - width / 2, y: tile.midY - height / 2, width: width, height: height))
        guard let png = bitmap.representation(using: .png, properties: [:]) else {
            throw NSError(domain: "GenerateIcons", code: 3, userInfo: [NSLocalizedDescriptionKey: "Could not encode PNG"])
        }
        return png
    }
}
