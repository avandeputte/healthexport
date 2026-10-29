import Foundation
import Network

/// Finds Mac receivers on the local network with Bonjour and streams NDJSON to the chosen one.
@MainActor
final class SyncClient: ObservableObject {
    struct Receiver: Identifiable, Hashable {
        let id: String
        let name: String
        let endpoint: NWEndpoint
        static func == (a: Receiver, b: Receiver) -> Bool { a.id == b.id }
        func hash(into h: inout Hasher) { h.combine(id) }
    }

    @Published private(set) var receivers: [Receiver] = []
    private var browser: NWBrowser?

    func startBrowsing() {
        let b = NWBrowser(for: .bonjour(type: SyncProtocol.bonjourType, domain: nil), using: .tcp)
        b.browseResultsChangedHandler = { [weak self] results, _ in
            let list = results.compactMap { r -> Receiver? in
                guard case .service(let name, _, _, _) = r.endpoint else { return nil }
                return Receiver(id: name, name: name, endpoint: r.endpoint)
            }.sorted { $0.name < $1.name }
            Task { @MainActor in self?.receivers = list }
        }
        b.start(queue: .global(qos: .userInitiated))
        browser = b
    }

    func stopBrowsing() { browser?.cancel(); browser = nil }

    func connect(to receiver: Receiver) async throws -> Connection {
        let params = NWParameters.tcp
        params.includePeerToPeer = true
        let c = Connection(NWConnection(to: receiver.endpoint, using: params))
        try await c.open()
        return c
    }

    /// One open TCP connection. `send` buffers lines and flushes in ~64 KB chunks, waiting for each chunk to be
    /// handed to the network before continuing, which is what stops a 2 GB history from piling up in memory.
    final class Connection {
        private let conn: NWConnection
        private var buffer = Data()
        private(set) var sent = 0
        private let flushThreshold = 64 * 1024

        init(_ conn: NWConnection) { self.conn = conn }

        func open() async throws {
            try await withCheckedThrowingContinuation { (k: CheckedContinuation<Void, Error>) in
                var resumed = false
                conn.stateUpdateHandler = { state in
                    guard !resumed else { return }
                    switch state {
                    case .ready: resumed = true; k.resume()
                    case .failed(let e): resumed = true; k.resume(throwing: e)
                    case .cancelled: resumed = true; k.resume(throwing: CancellationError())
                    default: break
                    }
                }
                conn.start(queue: .global(qos: .userInitiated))
            }
        }

        func send(_ message: SyncMessage) async throws {
            var line = try JSONEncoder.sync.encode(message)
            line.append(0x0A)
            buffer.append(line)
            sent += 1
            if buffer.count >= flushThreshold { try await flush() }
        }

        func flush() async throws {
            guard !buffer.isEmpty else { return }
            let data = buffer
            buffer = Data()
            try await withCheckedThrowingContinuation { (k: CheckedContinuation<Void, Error>) in
                conn.send(content: data, completion: .contentProcessed { e in
                    if let e { k.resume(throwing: e) } else { k.resume() }
                })
            }
        }

        /// Waits for the Mac's final `ack` line.
        func waitForAck() async throws -> Int {
            try await withCheckedThrowingContinuation { (k: CheckedContinuation<Int, Error>) in
                conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { data, _, _, error in
                    if let error { return k.resume(throwing: error) }
                    guard let data,
                          let line = String(data: data, encoding: .utf8)?.split(separator: "\n").first,
                          let msg = try? JSONDecoder.sync.decode(SyncMessage.self, from: Data(line.utf8)),
                          case .ack(let n) = msg else {
                        return k.resume(throwing: SyncError.badAck)
                    }
                    k.resume(returning: n)
                }
            }
        }

        func close() { conn.cancel() }
    }
}

enum SyncError: LocalizedError {
    case badAck
    var errorDescription: String? { "The Mac closed the connection without confirming the data." }
}
