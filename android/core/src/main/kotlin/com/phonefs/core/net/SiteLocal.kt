package com.phonefs.core.net

import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress

/** §7.5 暴露面：仅对站点本地/环回地址业务响应（W2 简化版，未做本机已发布地址比对）。 */
object SiteLocal {
    fun isAllowed(addr: InetAddress): Boolean {
        if (addr.isLoopbackAddress) return true // adb forward 功能联调放行
        if (addr.isSiteLocalAddress) return true // 10/8、172.16/12、192.168/16
        if (addr is Inet6Address) {
            val b = addr.address
            // fc00::/7（ULA）
            if ((b[0].toInt() and 0xFE) == 0xFC) return true
            // fe80::/10（link-local）
            if (b[0] == 0xFE.toByte() && (b[1].toInt() and 0xC0) == 0x80) return true
        }
        if (addr is Inet4Address) return false
        return false
    }
}
