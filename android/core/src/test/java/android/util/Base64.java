package android.util;

/** Local JVM test shim for the android.util.Base64 flags used by DocIds. */
public final class Base64 {
    public static final int NO_PADDING = 1;
    public static final int NO_WRAP = 2;
    public static final int URL_SAFE = 8;

    private Base64() {}

    public static String encodeToString(byte[] input, int flags) {
        java.util.Base64.Encoder encoder = (flags & URL_SAFE) != 0
            ? java.util.Base64.getUrlEncoder()
            : java.util.Base64.getEncoder();
        if ((flags & NO_PADDING) != 0) {
            encoder = encoder.withoutPadding();
        }
        return encoder.encodeToString(input);
    }

    public static byte[] decode(String input, int flags) {
        java.util.Base64.Decoder decoder = (flags & URL_SAFE) != 0
            ? java.util.Base64.getUrlDecoder()
            : java.util.Base64.getDecoder();
        return decoder.decode(input);
    }
}
