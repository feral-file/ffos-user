// macOS-only verification helper: decode compositor captures without modifying them.
import Foundation
import Vision
for path in CommandLine.arguments.dropFirst() {
    let request = VNDetectBarcodesRequest()
    request.symbologies = [.qr]
    do {
        try VNImageRequestHandler(url: URL(fileURLWithPath: path)).perform([request])
        let values = (request.results ?? []).compactMap { $0.payloadStringValue }
        let data = try JSONSerialization.data(withJSONObject: ["file": path, "qr": values], options: [.sortedKeys])
        print(String(data: data, encoding: .utf8)!)
        if values.isEmpty { exit(1) }
    } catch { fputs("QR verification failed: \(error)\n", stderr); exit(1) }
}
