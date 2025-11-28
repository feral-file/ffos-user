pub fn decode_varint(buf: &[u8]) -> Option<(u64, usize)> {
    const MAX_VARINT_LEN: usize = 10; // u64 needs at most 10 bytes
    let mut value = 0u64;
    let mut shift = 0;
    for (i, &byte) in buf.iter().take(MAX_VARINT_LEN).enumerate() {
        // Prevent overflow: u64 has 64 bits, so shift must be < 64
        if shift >= 64 {
            return None;
        }
        value |= ((byte & 0x7F) as u64) << shift;
        if byte & 0x80 == 0 {
            return Some((value, i + 1));
        }
        shift += 7;
    }
    None
}

/// Maximum allowed length for a single value in the payload (1MB)
const MAX_VALUE_LENGTH: u64 = 1024 * 1024;

/// Maximum number of values in a payload
const MAX_VALUE_COUNT: usize = 100;

/// Parse the payload into a list of values with safety checks:
pub fn parse_payload(buf: &[u8]) -> Option<Vec<String>> {
    let mut vals = Vec::new();
    let mut cursor = 0;

    while cursor < buf.len() {
        // Check if we've exceeded the maximum number of values
        if vals.len() >= MAX_VALUE_COUNT {
            eprintln!("BLE: Payload exceeds maximum value count of {MAX_VALUE_COUNT}");
            return None;
        }

        let (val_len, varint_len) = decode_varint(&buf[cursor..])?;

        // Check if the value length is reasonable
        if val_len > MAX_VALUE_LENGTH {
            eprintln!("BLE: Value length {val_len} exceeds maximum of {MAX_VALUE_LENGTH}");
            return None;
        }

        let val_start = cursor + varint_len;
        let val_end = val_start + val_len as usize;

        // Check bounds before slicing
        if val_end > buf.len() {
            eprintln!(
                "BLE: Value end {val_end} exceeds buffer length {}",
                buf.len()
            );
            return None;
        }

        let val = std::str::from_utf8(&buf[val_start..val_end])
            .ok()?
            .to_string();
        vals.push(val);
        cursor = val_end;
    }
    Some(vals)
}

pub fn encode_varint(mut value: u64) -> Vec<u8> {
    // the length of value in this app is usually small
    // so we don't want to pre-allocate a large buffer
    let mut buf = Vec::new();
    while value >= 0x80 {
        buf.push((value as u8) | 0x80);
        value >>= 7;
    }
    buf.push(value as u8);
    buf
}

pub struct PayloadEncoder {
    buf: Vec<u8>,
}

impl PayloadEncoder {
    pub fn new() -> Self {
        Self { buf: Vec::new() }
    }

    pub fn push_bytes(&mut self, bytes: &[u8]) {
        self.buf
            .extend_from_slice(&encode_varint(bytes.len() as u64));
        self.buf.extend_from_slice(bytes);
    }

    pub fn push_str(&mut self, s: &str) {
        self.push_bytes(s.as_bytes());
    }

    pub fn push_code(&mut self, code: u8) {
        self.push_bytes(&[code]);
    }

    pub fn finish(self) -> Vec<u8> {
        self.buf
    }
}
