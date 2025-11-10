pub fn decode_varint(buf: &[u8]) -> Option<(u64, usize)> {
    let mut value = 0u64;
    let mut shift = 0;
    for (i, &byte) in buf.iter().enumerate() {
        value |= ((byte & 0x7F) as u64) << shift;
        if byte & 0x80 == 0 {
            return Some((value, i + 1));
        }
        shift += 7;
    }
    None
}

/// Parse the payload into a list of values:
pub fn parse_payload(buf: &[u8]) -> Option<Vec<String>> {
    let mut vals = Vec::new();
    let mut cursor = 0;

    while cursor < buf.len() {
        let (val_len, varint_len) = decode_varint(&buf[cursor..])?;
        let val_start = cursor + varint_len;
        let val_end = val_start + val_len as usize;
        if val_end > buf.len() {
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

pub fn encode_payload(vals: &[&[u8]]) -> Vec<u8> {
    let mut buf = Vec::new();
    for val in vals {
        buf.extend_from_slice(&encode_varint(val.len() as u64));
        buf.extend_from_slice(val);
    }
    buf
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_varint_basic_values() {
        assert_eq!(decode_varint(&[0x00]), Some((0, 1)));
        assert_eq!(decode_varint(&[0x7F]), Some((127, 1)));
        assert_eq!(decode_varint(&[0x80, 0x01]), Some((128, 2)));
        assert_eq!(decode_varint(&[0xFF, 0x7F]), Some((16_383, 2)));
        assert_eq!(decode_varint(&[0x80, 0x80, 0x01]), Some((16_384, 3)));
    }

    #[test]
    fn decode_varint_handles_u64_max() {
        let encoded = encode_varint(u64::MAX);
        assert_eq!(decode_varint(&encoded), Some((u64::MAX, encoded.len())));
    }

    #[test]
    fn decode_varint_invalid_sequences_return_none() {
        assert_eq!(decode_varint(&[]), None);
        assert_eq!(decode_varint(&[0x80]), None); // continuation bit without termination
    }

    #[test]
    fn encode_varint_roundtrip() {
        let values = [0_u64, 1, 127, 128, 16_383, 16_384, u64::MAX];
        for &value in &values {
            let encoded = encode_varint(value);
            assert_eq!(
                decode_varint(&encoded),
                Some((value, encoded.len())),
                "failed roundtrip for value {value}"
            );
        }
    }

    #[test]
    fn parse_payload_roundtrip() {
        let parts = vec!["hello", "world", "utf8"];
        let encoded = encode_payload(&parts.iter().map(|s| s.as_bytes()).collect::<Vec<_>>());
        let decoded = parse_payload(&encoded).expect("payload should decode");
        assert_eq!(decoded, parts);
    }

    #[test]
    fn parse_payload_empty_buffer_returns_empty_vec() {
        assert_eq!(parse_payload(&[]), Some(Vec::new()));
    }

    #[test]
    fn parse_payload_invalid_length_returns_none() {
        // declare a payload with length 2 but only provide one byte
        let invalid_payload = vec![0x02, b'a'];
        assert!(parse_payload(&invalid_payload).is_none());
    }

    #[test]
    fn encode_payload_handles_empty_input() {
        let encoded = encode_payload(&[]);
        assert!(encoded.is_empty());
        assert_eq!(parse_payload(&encoded), Some(Vec::new()));
    }
}
