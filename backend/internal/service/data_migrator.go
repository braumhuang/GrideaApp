package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gridea-pro/backend/internal/domain"
	"gridea-pro/backend/internal/repository"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

// DataMigrator 负责在应用启动时全量清洗和迁移本地老旧格式的 ID
// 把不合规的分类和标签 ID 洗刷为统一的 6位 NanoID，并将文章（Post）中针对名字/别名的关联
// 统一投射、补足为符合当前字典映射的标准 CategoryIDs / TagIDs 数组。
type DataMigrator struct {
	appDir       string
	categoryRepo domain.CategoryRepository
	tagRepo      domain.TagRepository
	postRepo     domain.PostRepository
	menuRepo     domain.MenuRepository
	linkRepo     domain.LinkRepository
	memoRepo     domain.MemoRepository

	// NanoID 统一生成规范
	alphabet string
	length   int
}

func NewDataMigrator(
	appDir string,
	categoryRepo domain.CategoryRepository,
	tagRepo domain.TagRepository,
	postRepo domain.PostRepository,
	menuRepo domain.MenuRepository,
	linkRepo domain.LinkRepository,
	memoRepo domain.MemoRepository,
) *DataMigrator {
	return &DataMigrator{
		appDir:       appDir,
		categoryRepo: categoryRepo,
		tagRepo:      tagRepo,
		postRepo:     postRepo,
		menuRepo:     menuRepo,
		linkRepo:     linkRepo,
		memoRepo:     memoRepo,

		// 全局约束
		alphabet: "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
		length:   6,
	}
}

// generateID 内部统一下发 ID
func (m *DataMigrator) generateID() string {
	id, _ := gonanoid.Generate(m.alphabet, m.length)
	return id
}

// isValidID 检查当前 ID 是否符合 6位长度 以及纯粹的字母表规范
func (m *DataMigrator) isValidID(id string) bool {
	if len(id) != m.length {
		return false
	}
	// 简单校验是否都在 alphabet 内（可选，通常长度不对就足以判断是否是用老的方式例如 UUID 或 9 位等生成的）
	for _, char := range id {
		isValidChar := false
		for _, validChar := range m.alphabet {
			if char == validChar {
				isValidChar = true
				break
			}
		}
		if !isValidChar {
			return false
		}
	}
	return true
}

// migrateUnderscoreIdToId 将旧版 JSON 配置文件中的 "_id" key 迁移为 "id"
// 该方法必须在任何 Repository.List() 调用之前执行，因为 Repository 使用懒加载
func (m *DataMigrator) migrateUnderscoreIdToId() {
	configDir := filepath.Join(m.appDir, "config")
	files := []string{"tags.json", "categories.json", "links.json", "menus.json", "memos.json"}

	for _, fileName := range files {
		filePath := filepath.Join(configDir, fileName)
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue // 文件可能不存在，跳过
		}

		newData := bytes.ReplaceAll(data, []byte(`"_id"`), []byte(`"id"`))
		if !bytes.Equal(data, newData) {
			if err := repository.WriteFileAtomic(filePath, newData, 0644); err != nil {
				log.Printf("[DataMigrator] 迁移 _id -> id 失败 [%s]: %v", fileName, err)
			} else {
				log.Printf("[DataMigrator] 已迁移 _id -> id [%s]", fileName)
			}
		}
	}
}

func (m *DataMigrator) RunMigration(ctx context.Context) error {
	// 运行策略：正常启动（无历史脏数据）下完全静默，避免刷屏；
	// 只有真正修复了数据才在结尾打一条汇总。错误仍保留为 log.Printf，
	// 便于用户发现保存失败等异常。

	// ---------------- 第零步：将旧版 "_id" key 迁移为 "id" ----------------
	// migrateUnderscoreIdToId 内部只在发生实际替换时 log
	m.migrateUnderscoreIdToId()

	// ---------------- 第一步：基础数据清洗与映射构建 ----------------

	var categoryFixed, tagFixed, menuFixed, linkFixed, memoFixed, postFixed int

	// 1.1 获取并洗刷分类 (Category)
	categories, err := m.categoryRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("加载分类失败: %w", err)
	}

	categorySlugToIDMap := make(map[string]string) // 老 Slug -> 新 ID
	categoryNameToIDMap := make(map[string]string) // 老 Name -> 新 ID (作为备用冗余)
	var categoryNeedsSave bool

	for i, cat := range categories {
		if !m.isValidID(cat.ID) { // ID为空或长度不对
			categories[i].ID = m.generateID()
			categoryNeedsSave = true
			categoryFixed++
		}
		// 加入映射大表，无论原本是否合规，都需入表，方便供 Post 检索引用
		categorySlugToIDMap[categories[i].Slug] = categories[i].ID
		categoryNameToIDMap[categories[i].Name] = categories[i].ID
	}

	if categoryNeedsSave {
		// 因为很多老数据原来是没有 ID 的，如果调 Update 会报 "item not found"。所以必须用 SaveAll 全量覆盖保存。
		// 保存失败必须 fail-fast：若仅 log 后继续，下文 Post.Update 会用 in-memory 中
		// 已经赋了新 ID 的 categorySlugToIDMap，但磁盘上的 categories.json 还是旧的——
		// 文章会写入"只在内存中存在"的 CategoryID，重启后引用断裂、分类页丢文章。
		if err := m.categoryRepo.SaveAll(ctx, categories); err != nil {
			return fmt.Errorf("迁移分类数据保存失败: %w", err)
		}
	}

	// 1.2 获取并洗刷标签 (Tag)
	tags, err := m.tagRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("加载标签失败: %w", err)
	}

	tagNameToIDMap := make(map[string]string)
	var tagNeedsSave bool

	for i, tag := range tags {
		if !m.isValidID(tag.ID) {
			tags[i].ID = m.generateID()
			tagNeedsSave = true
			tagFixed++
		}
		tagNameToIDMap[tags[i].Name] = tags[i].ID
	}

	if tagNeedsSave {
		// 同 categories：失败必须 fail-fast，否则 Post.TagIDs 写入只在内存中存在的 ID。
		if err := m.tagRepo.SaveAll(ctx, tags); err != nil {
			return fmt.Errorf("迁移标签数据保存失败: %w", err)
		}
	}

	// 1.3 获取并洗刷菜单 (Menu)
	if menus, err := m.menuRepo.List(ctx); err == nil {
		var menuNeedsSave bool
		for i, menu := range menus {
			if !m.isValidID(menu.ID) {
				menus[i].ID = m.generateID()
				menuNeedsSave = true
				menuFixed++
			}
		}
		if menuNeedsSave {
			// menus/links/memos 不被 Post 引用，但 ID 仍需持久化——失败同样 fail-fast，
			// 让用户立刻看到目录写入异常（云盘锁文件、磁盘满等），不带不一致状态上路。
			if err := m.menuRepo.SaveAll(ctx, menus); err != nil {
				return fmt.Errorf("迁移菜单数据保存失败: %w", err)
			}
		}
	} else {
		log.Printf("[DataMigrator] 加载菜单失败 (跳过): %v", err)
	}

	// 1.4 获取并洗刷友链 (Link)
	if links, err := m.linkRepo.List(ctx); err == nil {
		var linkNeedsSave bool
		for i, link := range links {
			if !m.isValidID(link.ID) {
				links[i].ID = m.generateID()
				linkNeedsSave = true
				linkFixed++
			}
		}
		if linkNeedsSave {
			if err := m.linkRepo.SaveAll(ctx, links); err != nil {
				return fmt.Errorf("迁移友链数据保存失败: %w", err)
			}
		}
	} else {
		log.Printf("[DataMigrator] 加载友链失败 (跳过): %v", err)
	}

	// 1.5 获取并洗刷闪念 (Memo)
	if memos, err := m.memoRepo.List(ctx); err == nil {
		var memoNeedsSave bool
		for i, memo := range memos {
			if !m.isValidID(memo.ID) {
				memos[i].ID = m.generateID()
				memoNeedsSave = true
				memoFixed++
			}
		}
		if memoNeedsSave {
			if err := m.memoRepo.SaveAll(ctx, memos); err != nil {
				return fmt.Errorf("迁移闪念数据保存失败: %w", err)
			}
		}
	} else {
		log.Printf("[DataMigrator] 加载闪念失败 (跳过): %v", err)
	}

	// ---------------- 第二步：文章关联关系的修复 ----------------

	posts, err := m.postRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("加载文章聚合失败: %w", err)
	}

	// 单篇 post 回写失败用 errors.Join 累积，不直接 return：单个文件锁定 / 权限
	// 异常不应阻断其余文章的迁移，但所有失败仍需向上汇报，避免静默通过。
	var postUpdateErrs error

	for _, post := range posts {
		var postModified bool

		// 2.0 修复 Post 自带的 ID
		if !m.isValidID(post.ID) {
			post.ID = m.generateID()
			postModified = true
		}

		// 2.1 修复 CategoryIDs（文章老的 Categories 字段有值但与 CategoryIDs 对不上）
		if len(post.Categories) > 0 {
			var newCategoryIDs []string
			for _, catIdent := range post.Categories {
				// 按 Slug → Name → 原本就是 ID 的顺序尝试映射
				if mappedID, ok := categorySlugToIDMap[catIdent]; ok {
					newCategoryIDs = append(newCategoryIDs, mappedID)
					continue
				}
				if mappedID, ok := categoryNameToIDMap[catIdent]; ok {
					newCategoryIDs = append(newCategoryIDs, mappedID)
					continue
				}
				if m.isValidID(catIdent) {
					newCategoryIDs = append(newCategoryIDs, catIdent)
					continue
				}
			}
			// 只在顺序无关的元素集不同才算"需要覆写"，避免抖动
			if !slicesEqual(post.CategoryIDs, newCategoryIDs) {
				post.CategoryIDs = newCategoryIDs
				postModified = true
			}
		}

		// 2.2 修复 TagIDs
		if len(post.Tags) > 0 {
			var newTagIDs []string
			for _, tagName := range post.Tags {
				if mappedID, ok := tagNameToIDMap[tagName]; ok {
					newTagIDs = append(newTagIDs, mappedID)
				} else if m.isValidID(tagName) {
					// 兼容部分人直接把新 ID 填在 tags[] 里
					newTagIDs = append(newTagIDs, tagName)
				}
			}
			if !slicesEqual(post.TagIDs, newTagIDs) {
				post.TagIDs = newTagIDs
				postModified = true
			}
		}

		if postModified {
			if err := m.postRepo.Update(ctx, &post); err != nil {
				postUpdateErrs = errors.Join(postUpdateErrs,
					fmt.Errorf("回写修复文章失败 [%s]: %w", post.FileName, err))
			} else {
				postFixed++
			}
		}
	}

	// 只在有真实修复时打一条汇总；正常启动下完全静默
	total := categoryFixed + tagFixed + menuFixed + linkFixed + memoFixed + postFixed
	if total > 0 {
		log.Printf("[DataMigrator] 修复了 %d 项历史数据 (分类 %d / 标签 %d / 菜单 %d / 友链 %d / 闪念 %d / 文章 %d)",
			total, categoryFixed, tagFixed, menuFixed, linkFixed, memoFixed, postFixed)
	}
	return postUpdateErrs
}

// slicesEqual 比较两个 string 切片内容是否一致（无序集合比较，解决顺序不同导致的反复重写）
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	// 如果都是空，也是相等
	if len(a) == 0 {
		return true
	}

	countMap := make(map[string]int)
	for _, item := range a {
		countMap[item]++
	}

	for _, item := range b {
		countMap[item]--
		if countMap[item] < 0 {
			return false
		}
	}

	// 此时所有的 count 应该都降回 0 了，因为长度相等且没出现负数
	return true
}
