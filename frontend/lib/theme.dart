/// 主题与色板：架构与姊妹仓（upstream/relay）完全一致——
/// AppTokens 作为 ThemeExtension 提供浅/深两套，组件经 context.tokens 取色；
/// 非颜色常量集中在 AppConst。主色跟随本仓 LOGO 的琥珀橙，
/// 与 upstream 的翡翠绿、relay 的蓝形成家族内可区分的三色。
library;

import 'package:flutter/material.dart';

/// 非颜色常量(不随明暗变化)。
abstract final class AppConst {
  static const radiusCard = 16.0;
  static const radiusCtrl = 10.0;
  // 字体栈对齐 CC Switch 的 system-ui:拉丁/数字用 Segoe UI,
  // 中文经 fallback 落到雅黑(单设雅黑会让拉丁字形发闷)
  static const fontFamily = 'Segoe UI';
  static const fontFallback = ['Microsoft YaHei'];
  static const fontMono = 'Cascadia Code';
}

/// 主题色板(ThemeExtension):浅色/深色两套,组件经 context.tokens 取色。
/// 浅色向 CC Switch 靠拢:近白底、柔和边框;主色取 LOGO 的琥珀橙。
class AppTokens extends ThemeExtension<AppTokens> {
  final Color bg, surface, border, ink, dim, faint;
  final Color primary, primaryInk, primarySoft;
  final Color success, successSoft, warn, warnSoft, danger, dangerSoft;
  final Color violet;

  const AppTokens({
    required this.bg,
    required this.surface,
    required this.border,
    required this.ink,
    required this.dim,
    required this.faint,
    required this.primary,
    required this.primaryInk,
    required this.primarySoft,
    required this.success,
    required this.successSoft,
    required this.warn,
    required this.warnSoft,
    required this.danger,
    required this.dangerSoft,
    required this.violet,
  });

  /// 浅色(CC Switch 式:近白灰底、白卡、浅边框、琥珀橙主色)。
  static const light = AppTokens(
    bg: Color(0xFFF5F6F8),
    surface: Color(0xFFFFFFFF),
    border: Color(0xFFE4E7ED),
    ink: Color(0xFF171B24),
    dim: Color(0xFF57606E),
    faint: Color(0xFF7A8494),
    primary: Color(0xFFEA580C),
    primaryInk: Color(0xFFC2410C),
    primarySoft: Color(0xFFFDEBD9),
    success: Color(0xFF1E9E62),
    successSoft: Color(0xFFE4F6EC),
    warn: Color(0xFFC98A0B),
    warnSoft: Color(0xFFFBF3DE),
    danger: Color(0xFFC03535),
    dangerSoft: Color(0xFFFBEAEA),
    violet: Color(0xFF7A5AF8),
  );

  /// 深色(跟随系统)。
  static const dark = AppTokens(
    bg: Color(0xFF0F141C),
    surface: Color(0xFF161D29),
    border: Color(0xFF263040),
    ink: Color(0xFFE9EDF5),
    dim: Color(0xFFB4BDCC),
    faint: Color(0xFF98A2B6),
    primary: Color(0xFFFB923C),
    primaryInk: Color(0xFFFDBA74),
    primarySoft: Color(0xFF3A2413),
    success: Color(0xFF3FBF7F),
    successSoft: Color(0xFF15362B),
    warn: Color(0xFFE0A83C),
    warnSoft: Color(0xFF33270F),
    danger: Color(0xFFE26868),
    dangerSoft: Color(0xFF3D2020),
    violet: Color(0xFF9E8CFC),
  );

  @override
  AppTokens copyWith() => this; // 不可变,不支持部分覆盖

  @override
  AppTokens lerp(AppTokens? other, double t) => t < 0.5 ? this : other!;
}

extension AppTokensX on BuildContext {
  AppTokens get tokens => Theme.of(this).extension<AppTokens>()!;
}

TextStyle _fs(
  double size,
  Color color, {
  FontWeight weight = FontWeight.w400,
}) => TextStyle(
  fontSize: size,
  fontWeight: weight,
  color: color,
  fontFamily: AppConst.fontFamily,
  fontFamilyFallback: AppConst.fontFallback,
);

ThemeData _build(AppTokens t, Brightness brightness) {
  final isDark = brightness == Brightness.dark;
  final base = isDark
      ? ThemeData.dark(useMaterial3: true)
      : ThemeData.light(useMaterial3: true);
  final scheme = (isDark ? ColorScheme.dark : ColorScheme.light)(
    primary: t.primary,
    onPrimary: Colors.white,
    primaryContainer: t.primarySoft,
    onPrimaryContainer: t.primaryInk,
    // SegmentedButton 的选中态取这对容器色,与侧栏导航选中态同一语言。
    secondaryContainer: t.primarySoft,
    onSecondaryContainer: t.primaryInk,
    surface: t.bg,
    onSurface: t.ink,
    surfaceContainerHighest: t.surface,
    outline: t.border,
    outlineVariant: t.border,
    error: t.danger,
  );
  return base.copyWith(
    colorScheme: scheme,
    scaffoldBackgroundColor: t.bg,
    // DataTable 的行分隔线取 dividerColor,与卡片描边同色。
    dividerColor: t.border,
    textTheme: base.textTheme.apply(
      fontFamily: AppConst.fontFamily,
      fontFamilyFallback: AppConst.fontFallback,
    ),
    extensions: [t],
    cardTheme: CardThemeData(
      color: t.surface,
      elevation: 0.4,
      shadowColor: isDark ? Colors.transparent : const Color(0x0A171B24),
      margin: EdgeInsets.zero,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCard),
        side: BorderSide(color: t.border),
      ),
    ),
    appBarTheme: AppBarTheme(
      backgroundColor: t.surface,
      foregroundColor: t.ink,
      elevation: 0,
      scrolledUnderElevation: 0,
      centerTitle: false,
      titleTextStyle: _fs(15, t.ink, weight: FontWeight.w700),
    ),
    chipTheme: base.chipTheme.copyWith(
      backgroundColor: t.bg,
      side: BorderSide(color: t.border),
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(7)),
      labelStyle: _fs(11.5, t.dim),
      padding: const EdgeInsets.symmetric(horizontal: 6),
    ),
    dialogTheme: DialogThemeData(
      backgroundColor: t.surface,
      shape: RoundedRectangleBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCard),
      ),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        backgroundColor: t.primary,
        foregroundColor: Colors.white,
        // 显式钉字体族:按钮 textStyle 若缺 fontFamily,合并链上会丢掉
        // textTheme 的族设置回退 Roboto(中文 tofu)
        textStyle: _fs(13, Colors.white, weight: FontWeight.w600),
        padding: const EdgeInsets.symmetric(horizontal: 18, vertical: 12),
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        ),
      ),
    ),
    outlinedButtonTheme: OutlinedButtonThemeData(
      style: OutlinedButton.styleFrom(
        foregroundColor: t.dim,
        textStyle: _fs(13, t.dim, weight: FontWeight.w600),
        padding: const EdgeInsets.symmetric(horizontal: 18, vertical: 12),
        side: BorderSide(color: t.border),
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        ),
      ),
    ),
    inputDecorationTheme: InputDecorationTheme(
      isDense: true,
      filled: true,
      fillColor: t.surface,
      contentPadding: const EdgeInsets.symmetric(horizontal: 13, vertical: 11),
      border: OutlineInputBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        borderSide: BorderSide(color: t.border),
      ),
      enabledBorder: OutlineInputBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        borderSide: BorderSide(color: t.border),
      ),
      focusedBorder: OutlineInputBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        borderSide: BorderSide(color: t.primary, width: 1.5),
      ),
      errorBorder: OutlineInputBorder(
        borderRadius: BorderRadius.circular(AppConst.radiusCtrl),
        borderSide: BorderSide(color: t.danger),
      ),
      hintStyle: _fs(12, t.faint),
    ),
    dividerTheme: DividerThemeData(color: t.border, thickness: 1, space: 1),
    snackBarTheme: SnackBarThemeData(
      behavior: SnackBarBehavior.floating,
      backgroundColor: isDark ? const Color(0xFF263040) : null,
      contentTextStyle: isDark ? _fs(14, t.ink) : null,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(10)),
    ),
    progressIndicatorTheme: ProgressIndicatorThemeData(
      linearTrackColor: isDark
          ? const Color(0xFF263040)
          : const Color(0xFFC9D0DD),
    ),
    // 窗口选择器:选中态用主色软底,与侧栏导航选中态同一语言。
    segmentedButtonTheme: SegmentedButtonThemeData(
      style: ButtonStyle(
        visualDensity: VisualDensity.compact,
        textStyle: WidgetStatePropertyAll(_fs(12.5, t.dim)),
        side: WidgetStatePropertyAll(BorderSide(color: t.border)),
        backgroundColor: WidgetStatePropertyAll(t.surface),
        foregroundColor: WidgetStatePropertyAll(t.dim),
        shape: WidgetStatePropertyAll(
          RoundedRectangleBorder(borderRadius: BorderRadius.circular(8)),
        ),
        padding: const WidgetStatePropertyAll(
          EdgeInsets.symmetric(horizontal: 4),
        ),
      ),
    ),
    // 表格:表头弱色小字、数据行墨色,横线用 border 色,去掉 Material 默认灰底。
    dataTableTheme: DataTableThemeData(
      headingTextStyle: _fs(12, t.faint, weight: FontWeight.w600),
      dataTextStyle: _fs(12.5, t.ink),
      headingRowColor: const WidgetStatePropertyAll(Colors.transparent),
      dataRowColor: const WidgetStatePropertyAll(Colors.transparent),
      headingRowHeight: 38,
      dataRowMinHeight: 40,
      dataRowMaxHeight: 48,
      columnSpacing: 18,
      horizontalMargin: 16,
      dividerThickness: 1,
    ),
  );
}

ThemeData buildAppTheme() => _build(AppTokens.light, Brightness.light);
ThemeData buildAppDarkTheme() => _build(AppTokens.dark, Brightness.dark);
