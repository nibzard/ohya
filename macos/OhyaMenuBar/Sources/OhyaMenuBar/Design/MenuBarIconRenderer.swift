import AppKit
import CoreGraphics
import Foundation
import OhyaMenuBarCore

@MainActor
enum MenuBarIconRenderer {
    private static var imageCache: [MenuBarIconState: NSImage] = [:]

    static func render(state: MenuBarIconState) -> NSImage {
        if let cached = imageCache[state] {
            return cached
        }
        let size = NSSize(width: 18, height: 18)
        let parsed = cachedTemplateSVG

        let image = NSImage(size: size, flipped: true) { rect in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }

            if state == .paused {
                ctx.setAlpha(0.45)
            }

            if let parsed {
                ctx.saveGState()
                let scale = rect.width / 64.0
                ctx.scaleBy(x: scale, y: scale)
                ctx.translateBy(x: 3.4, y: -4.8)
                ctx.scaleBy(x: 1.12, y: 1.12)

                ctx.beginTransparencyLayer(auxiliaryInfo: nil)
                ctx.setFillColor(NSColor.black.cgColor)
                ctx.addPath(parsed.earPath)
                ctx.fillPath()

                ctx.setBlendMode(.destinationOut)
                for cut in parsed.maskCutouts {
                    ctx.addPath(cut)
                    ctx.fillPath()
                }
                ctx.endTransparencyLayer()

                ctx.setBlendMode(.normal)
                ctx.setFillColor(NSColor.black.cgColor)
                ctx.addPath(parsed.headPath)
                ctx.fillPath()
                ctx.restoreGState()
            } else if let fallback = NSImage(systemSymbolName: "brain.head.profile", accessibilityDescription: nil) {
                fallback.draw(in: rect.insetBy(dx: 1, dy: 1))
            }

            ctx.setAlpha(1.0)
            drawBadge(state: state, in: ctx, rect: rect)
            return true
        }
        image.isTemplate = true
        image.accessibilityDescription = state.accessibilityLabel
        imageCache[state] = image
        return image
    }

    private static func drawBadge(state: MenuBarIconState, in ctx: CGContext, rect: NSRect) {
        switch state {
        case .caughtUp, .paused:
            return
        case .syncing:
            let dot = CGRect(x: rect.maxX - 4.5, y: rect.maxY - 4.5, width: 4.0, height: 4.0)
            ctx.setBlendMode(.destinationOut)
            ctx.fillEllipse(in: dot.insetBy(dx: -1.1, dy: -1.1))
            ctx.setBlendMode(.normal)
            ctx.setFillColor(NSColor.black.cgColor)
            ctx.fillEllipse(in: dot)
        case .degraded, .unavailable:
            let badge = CGRect(x: rect.maxX - 6.0, y: rect.maxY - 6.0, width: 5.8, height: 5.8)
            ctx.setBlendMode(.destinationOut)
            ctx.fillEllipse(in: badge.insetBy(dx: -1.1, dy: -1.1))
            ctx.setBlendMode(.normal)
            ctx.setFillColor(NSColor.black.cgColor)
            ctx.fillEllipse(in: badge)
            ctx.setBlendMode(.destinationOut)
            ctx.fill(CGRect(x: badge.midX - 0.55, y: badge.minY + 1.1, width: 1.1, height: 2.2))
            ctx.fillEllipse(in: CGRect(x: badge.midX - 0.6, y: badge.maxY - 1.9, width: 1.2, height: 1.2))
            ctx.setBlendMode(.normal)
        }
    }

    private struct ParsedTemplateSVG: @unchecked Sendable {
        let maskCutouts: [CGPath]
        let earPath: CGPath
        let headPath: CGPath
    }

    private static let cachedTemplateSVG: ParsedTemplateSVG? = {
        guard let url = locateTemplateSVG(),
              let text = try? String(contentsOf: url, encoding: .utf8)
        else {
            return nil
        }
        let dValues = extractPathData(from: text)
        guard dValues.count >= 4 else { return nil }
        let cutouts = [parseSVGPath(dValues[0]), parseSVGPath(dValues[1])]
        let ear = parseSVGPath(dValues[2])
        let head = parseSVGPath(dValues[3])
        return ParsedTemplateSVG(maskCutouts: cutouts, earPath: ear, headPath: head)
    }()

    private static func locateTemplateSVG() -> URL? {
        if let url = Bundle.module.url(forResource: "menubar-template", withExtension: "svg") {
            return url
        }
        if let url = Bundle.main.url(forResource: "menubar-template", withExtension: "svg") {
            return url
        }
        return nil
    }

    private static func extractPathData(from svg: String) -> [String] {
        var results: [String] = []
        var searchStart = svg.startIndex
        let prefix = "<path d=\""
        while let range = svg.range(of: prefix, range: searchStart..<svg.endIndex) {
            let valueStart = range.upperBound
            guard let endQuote = svg[valueStart...].firstIndex(of: "\"") else { break }
            results.append(String(svg[valueStart..<endQuote]))
            searchStart = svg.index(after: endQuote)
        }
        return results
    }

    private static func parseSVGPath(_ d: String) -> CGPath {
        let path = CGMutablePath()
        let scanner = SVGTokenScanner(d)
        var current = CGPoint.zero

        while let cmd = scanner.nextCommand() {
            switch cmd {
            case "M":
                if let x = scanner.nextDouble(), let y = scanner.nextDouble() {
                    current = CGPoint(x: x, y: y)
                    path.move(to: current)
                }
            case "L":
                if let x = scanner.nextDouble(), let y = scanner.nextDouble() {
                    current = CGPoint(x: x, y: y)
                    path.addLine(to: current)
                }
            case "a":
                if let rx = scanner.nextDouble(),
                   let ry = scanner.nextDouble(),
                   let _ = scanner.nextDouble(),
                   let _ = scanner.nextDouble(),
                   let _ = scanner.nextDouble(),
                   let dx = scanner.nextDouble(),
                   let dy = scanner.nextDouble()
                {
                    let end = CGPoint(x: current.x + dx, y: current.y + dy)
                    let center = CGPoint(x: (current.x + end.x) / 2.0, y: (current.y + end.y) / 2.0)
                    let radius = (abs(rx) + abs(ry)) / 2.0
                    let startAngle = atan2(current.y - center.y, current.x - center.x)
                    path.addArc(center: center, radius: radius, startAngle: startAngle, endAngle: startAngle + .pi, clockwise: true)
                    current = end
                }
            case "Z", "z":
                path.closeSubpath()
            default:
                break
            }
        }
        return path
    }
}

private final class SVGTokenScanner {
    private let scalars: [UnicodeScalar]
    private var index = 0

    init(_ text: String) {
        self.scalars = Array(text.unicodeScalars)
    }

    func nextCommand() -> Character? {
        skipSeparators()
        guard index < scalars.count else { return nil }
        let scalar = scalars[index]
        if CharacterSet.letters.contains(scalar) {
            index += 1
            return Character(scalar)
        }
        return nil
    }

    func nextDouble() -> Double? {
        skipSeparators()
        guard index < scalars.count else { return nil }
        let start = index
        if scalars[index] == "-" || scalars[index] == "+" {
            index += 1
        }
        var seenDigit = false
        while index < scalars.count {
            let s = scalars[index]
            if (s >= "0" && s <= "9") || s == "." {
                seenDigit = true
                index += 1
            } else {
                break
            }
        }
        guard seenDigit else {
            index = start
            return nil
        }
        let str = String(String.UnicodeScalarView(scalars[start..<index]))
        return Double(str)
    }

    private func skipSeparators() {
        while index < scalars.count {
            let s = scalars[index]
            if s == " " || s == "," || s == "\n" || s == "\t" || s == "\r" {
                index += 1
            } else {
                break
            }
        }
    }
}
