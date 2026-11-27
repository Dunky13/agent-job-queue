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
        for (name, size, rounded) in [("remote/static/app-icon.png", 256, true), ("remote/static/favicon.png", 64, true),
                                      ("remote/static/apple-touch-icon.png", 180, false),
                                      ("site/img/icon.png", 112, true), ("site/favicon.png", 64, true)] {
            try render(size: size, desktop: false, rounded: rounded).write(to: root.appendingPathComponent(name))
        }
    }

    @MainActor
    static func render(size: Int, desktop: Bool, rounded: Bool) throws -> Data {
        guard let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
                                            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                                            isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
              let context = NSGraphicsContext(bitmapImageRep: bitmap) else {
            throw NSError(domain: "GenerateIcons", code: 2, userInfo: [NSLocalizedDescriptionKey: "Could not create the icon bitmap"])
        }
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = context
        defer { NSGraphicsContext.restoreGraphicsState() }
        context.cgContext.scaleBy(x: CGFloat(size) / 1024, y: CGFloat(size) / 1024)
        context.cgContext.clear(CGRect(x: 0, y: 0, width: 1024, height: 1024))
        let inset: CGFloat = desktop ? 80 : 0
        let tile = NSRect(x: inset, y: inset, width: 1024 - 2 * inset, height: 1024 - 2 * inset)
        NSColor(srgbRed: 0, green: 0.4, blue: 0.8, alpha: 1).setFill()
        NSBezierPath(roundedRect: tile, xRadius: rounded ? tile.width * 0.22 : 0,
                     yRadius: rounded ? tile.height * 0.22 : 0).fill()
        // The mark is drawn y-down on a 16-point grid; flip it and centre it in the tile.
        let side = tile.width * 0.7
        context.cgContext.translateBy(x: tile.midX - side / 2, y: tile.midY + side / 2)
        context.cgContext.scaleBy(x: 1, y: -1)
        QueueBrand.draw(.running, scale: side / 16, color: .white)
        guard let png = bitmap.representation(using: .png, properties: [:]) else {
            throw NSError(domain: "GenerateIcons", code: 3, userInfo: [NSLocalizedDescriptionKey: "Could not encode PNG"])
        }
        return png
    }
}
