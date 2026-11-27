import AppKit

// Shared by the app, installer, and icon asset generator.
enum QueueBrand {
    /// The queue's own mark: a plate on two stacked layers, drawn on a 16-point grid.
    /// The top plate carries the state, so the outline stays the same in every state.
    enum Mark: Sendable, CaseIterable {
        case idle, running, paused, draining, attention
    }

    /// A one-colour template image, tinted by the menu bar or the view like an SF Symbol.
    static func image(_ mark: Mark = .running, height: CGFloat = 16) -> NSImage {
        // AppKit may draw an image on any thread.
        let image = NSImage(size: NSSize(width: height, height: height), flipped: true) { @Sendable _ in
            draw(mark, scale: height / 16)
            return true
        }
        image.isTemplate = true
        return image
    }

    /// Draws the mark on the current context, flipped (y down), at 16 points times `scale`.
    static func draw(_ mark: Mark, scale: CGFloat, color: NSColor = .black) {
        let transform = AffineTransform(scale: scale)
        func stroked(_ points: [NSPoint], closed: Bool, dashed: Bool = false) {
            let path = NSBezierPath()
            path.move(to: points[0])
            points.dropFirst().forEach(path.line(to:))
            if closed { path.close() }
            path.transform(using: transform)
            path.lineWidth = 1.5 * scale
            path.lineCapStyle = .round
            path.lineJoinStyle = .round
            if dashed { path.setLineDash([1.6 * scale, 1.6 * scale], count: 2, phase: 0) }
            path.stroke()
        }
        color.set()
        stroked([NSPoint(x: 2, y: 8), NSPoint(x: 8, y: 11), NSPoint(x: 14, y: 8)], closed: false)
        stroked([NSPoint(x: 2, y: 11), NSPoint(x: 8, y: 14), NSPoint(x: 14, y: 11)], closed: false)
        switch mark {
        case .idle, .draining:
            // Outlined while nothing runs; dashed while intake closes.
            stroked([NSPoint(x: 8, y: 2), NSPoint(x: 14, y: 5), NSPoint(x: 8, y: 8), NSPoint(x: 2, y: 5)], closed: true, dashed: mark == .draining)
        case .running, .paused, .attention:
            let plate = NSBezierPath()
            plate.windingRule = .evenOdd
            plate.move(to: NSPoint(x: 8, y: 1.6))
            [NSPoint(x: 14.6, y: 5), NSPoint(x: 8, y: 8.4), NSPoint(x: 1.4, y: 5)].forEach(plate.line(to:))
            plate.close()
            // Even-odd fill cuts these out of the plate.
            let holes: [NSRect] = switch mark {
            case .paused: [NSRect(x: 6.6, y: 3.6, width: 1, height: 2.8), NSRect(x: 8.4, y: 3.6, width: 1, height: 2.8)]
            case .attention: [NSRect(x: 7.4, y: 3.2, width: 1.2, height: 2.2), NSRect(x: 7.4, y: 6, width: 1.2, height: 1)]
            default: []
            }
            holes.forEach { plate.appendRect($0) }
            plate.transform(using: transform)
            plate.fill()
        }
    }
}
